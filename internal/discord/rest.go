package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// UserAgent is required by Discord and should identify the project.
const UserAgent = "DiscordBot (https://github.com/cartpauj/meshycord, 0.0.1)"

// APIError is a non-2xx response from Discord, carrying enough to act on.
type APIError struct {
	Status  int
	Code    int    `json:"code"`
	Message string `json:"message"`
	Method  string
	Path    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("discord: %s %s -> %d (code %d: %s)", e.Method, e.Path, e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("discord: %s %s -> %d", e.Method, e.Path, e.Status)
}

// NotFound reports whether the resource is gone — a channel deleted by hand,
// most often. The caller should drop the route rather than retrying forever.
func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

// Forbidden reports a permissions problem, which is nearly always a missing
// bot permission rather than a bug.
func (e *APIError) Forbidden() bool { return e.Status == http.StatusForbidden }

// IsNotFound unwraps err and reports whether it is a 404.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.NotFound()
}

// IsForbidden unwraps err and reports whether it is a 403.
func IsForbidden(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Forbidden()
}

// Client is the REST half of the Discord adapter.
type Client struct {
	// Token is read through a function so that changing the bot token in the
	// web UI takes effect without a restart.
	Token func() string
	HTTP  *http.Client
	Log   *slog.Logger
	// BaseURL is the API root. Overridden only by tests, which point it at a
	// local server so request shapes can be asserted rather than assumed.
	BaseURL string

	limiter    rateLimiter
	authFailed atomic.Bool
	// global holds every request when Discord signals a global rate limit.
	globalUntil atomic.Int64
}

// NewClient builds a REST client.
func NewClient(token func() string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		Token: token,
		Log:   log,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				// One persistent connection is plenty and keeps TLS handshakes
				// — measured at 1.1s on an ARMv6 Pi Zero — off the hot path.
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 4,
				// Deliberately shorter than the 90s default, and shorter than
				// the idle timeout of whatever sits in front of Discord.
				//
				// A pooled connection the far end has already dropped is worse
				// than no pooled connection at all: Go will not retry a POST on
				// it, because POST is not idempotent, so the request just
				// fails. That is fatal for an interaction response, which has a
				// hard three-second deadline and no second chance — Discord
				// shows "the application did not respond in time".
				IdleConnTimeout:     25 * time.Second,
				ForceAttemptHTTP2:   true,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		},
	}
}

// KeepWarm holds a live connection to Discord open.
//
// Two problems, one answer. A TLS handshake costs about a second on ARMv6, and
// an interaction response must be sent within three — so paying for a
// handshake on the click is most of the budget gone. And a connection left
// idle long enough gets dropped by the far end, after which the next POST
// fails outright rather than transparently retrying.
//
// A cheap unauthenticated GET on a short cycle avoids both: the pool always
// holds a connection that is known good and already negotiated.
func (c *Client) KeepWarm(ctx context.Context) {
	// Comfortably inside IdleConnTimeout, so the connection is refreshed
	// before Go would discard it and long before the far end would.
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, APIBase+"/gateway", nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", UserAgent)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			c.Log.Debug("keep-warm request failed", "err", err)
			continue
		}
		// The body must be drained and closed or the connection is not
		// returned to the pool, which defeats the whole exercise.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
}

// AuthFailed reports whether Discord last rejected our credentials.
//
// Latched deliberately: without it a revoked token just makes everything fail
// silently forever, with nothing in the UI to explain why.
func (c *Client) AuthFailed() bool { return c.authFailed.Load() }

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// Discord rate-limits per "bucket", which is roughly a route with its major
// parameters. Rather than parse the bucket header dance perfectly, we keep one
// lock per route key: requests to the same route serialise, and each records
// what the response headers said about remaining budget.
//
// Ignoring a 429 earns longer bans, so retry_after is always honoured.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	mu        sync.Mutex
	remaining int
	resetAt   time.Time
	known     bool
}

func (r *rateLimiter) get(key string) *bucket {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.buckets == nil {
		r.buckets = map[string]*bucket{}
	}
	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{}
		r.buckets[key] = b
	}
	return b
}

// bucketKey collapses a path to its route shape, keeping the major parameters
// (the id right after channels/guilds/webhooks) because Discord's limits are
// per channel and per guild.
func bucketKey(method, path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")

	// Reactions are one bucket per channel, covering every emoji, every
	// message, and both PUT and DELETE. Discord enforces this far more tightly
	// than the ordinary message limit — roughly one request every 250ms — and
	// keying them any other way is not a small inefficiency:
	//
	// The emoji sits in the path and is not numeric, so it survived the
	// normalisation below and minted a NEW bucket for every emoji. A fresh
	// bucket is "unknown", an unknown bucket never waits, so each one fired
	// immediately, got a 429, and slept off the server's retry_after — a full
	// second, every single time. Three reactions per message (hourglass on,
	// hourglass off, verdict on) meant about three seconds of self-inflicted
	// delay on top of the mesh round trip, which is what made a two-second
	// delivery feel like it was hanging.
	//
	// Collapsed like this the limiter reads the headers off the first call and
	// paces the rest, so the 429 stops happening at all.
	for i, p := range parts {
		if p == "reactions" && i >= 1 {
			return "REACTIONS /" + strings.Join(parts[:2], "/") + "/reactions"
		}
	}

	for i, p := range parts {
		// Interaction and webhook tokens are unique per use, so leaving them in
		// the key mints a new bucket every time and grows the map without
		// bound. Both routes put the token third: /interactions/{id}/{token}/…
		// and /webhooks/{app}/{token}/….
		if i == 2 && !isNumeric(p) && (parts[0] == "interactions" || parts[0] == "webhooks") {
			parts[i] = "{token}"
			continue
		}
		if i == 0 || !isNumeric(p) {
			continue
		}
		switch parts[i-1] {
		case "channels", "guilds", "webhooks":
			// Major parameter: keep it, the limit really is per-object.
		default:
			parts[i] = "{id}"
		}
	}
	return method + " /" + strings.Join(parts, "/")
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Request plumbing
// ---------------------------------------------------------------------------

// do performs one request, honouring rate limits and retrying a 429 once.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	token := ""
	if c.Token != nil {
		token = c.Token()
	}
	if token == "" {
		return errors.New("discord: no bot token configured")
	}

	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("discord: encode request: %w", err)
		}
	}

	b := c.limiter.get(bucketKey(method, path))
	b.mu.Lock()
	defer b.mu.Unlock()

	const maxAttempts = 3
	for attempt := 1; ; attempt++ {
		if err := c.waitGlobal(ctx); err != nil {
			return err
		}
		if err := b.wait(ctx); err != nil {
			return err
		}

		status, respBody, retryAfter, err := c.roundTrip(ctx, method, path, token, payload, b)
		if err != nil {
			return err
		}

		switch {
		case status == http.StatusTooManyRequests:
			if attempt >= maxAttempts {
				return &APIError{Status: status, Method: method, Path: path,
					Message: "rate limited repeatedly"}
			}
			c.Log.Warn("rate limited by Discord", "path", path, "wait", retryAfter)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryAfter):
			}
			continue

		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			// 403 is usually a missing permission on one channel rather than a
			// dead token, so only 401 latches the auth-failed flag.
			if status == http.StatusUnauthorized && !c.authFailed.Swap(true) {
				c.Log.Error("Discord rejected the bot token: check it is correct and the bot is still in the server")
			}
			return decodeAPIError(status, method, path, respBody)

		case status >= 200 && status < 300:
			c.authFailed.Store(false)
			if out != nil && len(respBody) > 0 {
				if err := json.Unmarshal(respBody, out); err != nil {
					return fmt.Errorf("discord: decode %s %s: %w", method, path, err)
				}
			}
			return nil

		case status >= 500 && attempt < maxAttempts:
			// Discord's own trouble. Back off and try again rather than
			// reporting a failure the user cannot act on.
			wait := time.Duration(attempt) * time.Second
			c.Log.Debug("Discord server error; retrying", "status", status, "path", path, "in", wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue

		default:
			return decodeAPIError(status, method, path, respBody)
		}
	}
}

func (c *Client) roundTrip(ctx context.Context, method, path, token string, payload []byte, b *bucket) (int, []byte, time.Duration, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	base := c.BaseURL
	if base == "" {
		base = APIBase
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("User-Agent", UserAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("discord: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Bodies are small (a message, a channel). 1 MB is a generous ceiling that
	// still stops a pathological response eating memory on a Pi Zero.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, 0, fmt.Errorf("discord: read %s %s: %w", method, path, err)
	}

	b.observe(resp.Header)
	retryAfter := parseRetryAfter(resp.Header, respBody)
	if resp.Header.Get("X-RateLimit-Global") == "true" {
		c.globalUntil.Store(time.Now().Add(retryAfter).UnixMilli())
	}
	return resp.StatusCode, respBody, retryAfter, nil
}

func (c *Client) waitGlobal(ctx context.Context) error {
	until := c.globalUntil.Load()
	if until == 0 {
		return nil
	}
	d := time.Until(time.UnixMilli(until))
	if d <= 0 {
		return nil
	}
	c.Log.Warn("globally rate limited by Discord", "wait", d)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// wait blocks until this bucket has budget again.
func (b *bucket) wait(ctx context.Context) error {
	if !b.known || b.remaining > 0 {
		return nil
	}
	d := time.Until(b.resetAt)
	if d <= 0 {
		return nil
	}
	if d > 30*time.Second {
		d = 30 * time.Second // a nonsense reset header should not wedge us
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (b *bucket) observe(h http.Header) {
	rem := h.Get("X-RateLimit-Remaining")
	reset := h.Get("X-RateLimit-Reset-After")
	if rem == "" {
		return
	}
	n, err := strconv.Atoi(rem)
	if err != nil {
		return
	}
	b.known = true
	b.remaining = n
	if secs, err := strconv.ParseFloat(reset, 64); err == nil {
		b.resetAt = time.Now().Add(time.Duration(secs * float64(time.Second)))
	}
}

func parseRetryAfter(h http.Header, body []byte) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			return clampWait(secs)
		}
	}
	var payload struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.RetryAfter > 0 {
		return clampWait(payload.RetryAfter)
	}
	return time.Second
}

func clampWait(secs float64) time.Duration {
	if secs < 0.1 {
		secs = 0.1
	}
	if secs > 60 {
		secs = 60
	}
	return time.Duration(secs * float64(time.Second))
}

func decodeAPIError(status int, method, path string, body []byte) error {
	e := &APIError{Status: status, Method: method, Path: path}
	_ = json.Unmarshal(body, e)
	return e
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// SendMessage posts to a channel and returns the created message.
//
// The id matters: it is what a reaction is later attached to, so callers that
// want to mark delivery must keep it.
func (c *Client) SendMessage(ctx context.Context, channelID, content string) (*Message, error) {
	return c.SendMessageWith(ctx, channelID, CreateMessage{Content: content})
}

// CreateMessage is the body of a message post.
type CreateMessage struct {
	Content          string            `json:"content,omitempty"`
	MessageReference *MessageReference `json:"message_reference,omitempty"`
	Components       []Component       `json:"components,omitempty"`
	Flags            int               `json:"flags,omitempty"`
	// AllowedMentions is always set by SendMessageWith to suppress pings:
	// relayed mesh text is not trusted input and must not be able to @everyone
	// a Discord server.
	AllowedMentions *allowedMentions `json:"allowed_mentions,omitempty"`
}

type allowedMentions struct {
	Parse []string `json:"parse"`
}

// SendMessageWith posts a message with full control over the body.
func (c *Client) SendMessageWith(ctx context.Context, channelID string, m CreateMessage) (*Message, error) {
	if channelID == "" {
		return nil, errors.New("discord: no channel to post to")
	}
	if m.AllowedMentions == nil {
		// Empty parse list: no @everyone, no role pings, no user pings from
		// relayed text. Someone on the radio should not be able to notify a
		// whole Discord server.
		m.AllowedMentions = &allowedMentions{Parse: []string{}}
	}
	var out Message
	if err := c.do(ctx, http.MethodPost, "/channels/"+channelID+"/messages", m, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMessage fetches one message, for when a reply did not inline the message
// it replied to.
func (c *Client) GetMessage(ctx context.Context, channelID, messageID string) (*Message, error) {
	var out Message
	if err := c.do(ctx, http.MethodGet, "/channels/"+channelID+"/messages/"+messageID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EditMessage rewrites one of the bot's own messages.
func (c *Client) EditMessage(ctx context.Context, channelID, messageID, content string) error {
	body := map[string]any{"content": content, "allowed_mentions": allowedMentions{Parse: []string{}}}
	return c.do(ctx, http.MethodPatch, "/channels/"+channelID+"/messages/"+messageID, body, nil)
}

// DeleteMessage removes a message.
//
// Deleting a message the bot did not write needs MANAGE_MESSAGES. The caller
// must treat a failure as "the content is still visible" and say so, rather
// than assuming a secret was cleaned up.
func (c *Client) DeleteMessage(ctx context.Context, channelID, messageID string) error {
	err := c.do(ctx, http.MethodDelete, "/channels/"+channelID+"/messages/"+messageID, nil, nil)
	if IsNotFound(err) {
		return nil // already gone is a success
	}
	return err
}

// ---------------------------------------------------------------------------
// Reactions
// ---------------------------------------------------------------------------

// React adds the bot's own reaction. Needs no special permission.
func (c *Client) React(ctx context.Context, channelID, messageID, emoji string) error {
	path := "/channels/" + channelID + "/messages/" + messageID +
		"/reactions/" + url.PathEscape(emoji) + "/@me"
	return c.do(ctx, http.MethodPut, path, nil, nil)
}

// UnreactUser removes somebody else's reaction.
//
// Needs MANAGE_MESSAGES, unlike removing our own. Used to consume a reaction
// that was meant as a button press rather than as a piece of state: leaving it
// there would sit alongside the result and, worse, mean pressing it again does
// nothing because the reaction is already present.
func (c *Client) UnreactUser(ctx context.Context, channelID, messageID, emoji, userID string) error {
	path := "/channels/" + channelID + "/messages/" + messageID +
		"/reactions/" + url.PathEscape(emoji) + "/" + userID
	err := c.do(ctx, http.MethodDelete, path, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// Unreact removes the bot's OWN reaction.
//
// Deliberately the "@me" route: removing somebody else's reaction would need
// MANAGE_MESSAGES, and the bridge should not require it just to tidy up after
// itself.
func (c *Client) Unreact(ctx context.Context, channelID, messageID, emoji string) error {
	path := "/channels/" + channelID + "/messages/" + messageID +
		"/reactions/" + url.PathEscape(emoji) + "/@me"
	err := c.do(ctx, http.MethodDelete, path, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// Channels
// ---------------------------------------------------------------------------

// GetChannel fetches one channel.
func (c *Client) GetChannel(ctx context.Context, channelID string) (*Channel, error) {
	var out Channel
	if err := c.do(ctx, http.MethodGet, "/channels/"+channelID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChannelExists reports whether a stored channel id is still real.
//
// Stored ids must be verified, not trusted: a channel deleted by hand would
// otherwise never be recreated, because the create step is skipped whenever an
// id is on file.
func (c *Client) ChannelExists(ctx context.Context, channelID string) bool {
	if channelID == "" {
		return false
	}
	_, err := c.GetChannel(ctx, channelID)
	return err == nil
}

// GuildChannels lists every channel in the server.
func (c *Client) GuildChannels(ctx context.Context, guildID string) ([]Channel, error) {
	var out []Channel
	if err := c.do(ctx, http.MethodGet, "/guilds/"+guildID+"/channels", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateChannelRequest is the body of a channel creation.
type CreateChannelRequest struct {
	Name     string `json:"name"`
	Type     int    `json:"type"`
	Topic    string `json:"topic,omitempty"`
	ParentID string `json:"parent_id,omitempty"`
}

// CreateChannel makes a text channel or a category.
func (c *Client) CreateChannel(ctx context.Context, guildID string, req CreateChannelRequest) (*Channel, error) {
	if guildID == "" {
		return nil, errors.New("discord: no server (guild) id configured")
	}
	var out Channel
	if err := c.do(ctx, http.MethodPost, "/guilds/"+guildID+"/channels", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteChannel removes a channel or a category.
func (c *Client) DeleteChannel(ctx context.Context, channelID string) error {
	err := c.do(ctx, http.MethodDelete, "/channels/"+channelID, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// Application and interactions
// ---------------------------------------------------------------------------

// Application is the bot's own application record.
type Application struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CurrentApplication reads the bot's application, whose id is needed to
// register slash commands.
func (c *Client) CurrentApplication(ctx context.Context) (*Application, error) {
	var out Application
	if err := c.do(ctx, http.MethodGet, "/applications/@me", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegisterGuildCommands replaces the bot's slash commands in one server.
//
// Guild-scoped rather than global: guild commands appear instantly, while
// global ones can take an hour to propagate, which makes setup feel broken.
func (c *Client) RegisterGuildCommands(ctx context.Context, appID, guildID string, cmds []AppCommand) error {
	path := "/applications/" + appID + "/guilds/" + guildID + "/commands"
	return c.do(ctx, http.MethodPut, path, cmds, nil)
}

// RespondInteraction answers an interaction. This must happen within 3
// seconds or Discord shows "the application did not respond" — for anything
// slower, defer first and edit the reply afterwards.
func (c *Client) RespondInteraction(ctx context.Context, id, token string, r InteractionResponse) error {
	started := time.Now()
	err := c.do(ctx, http.MethodPost, "/interactions/"+id+"/"+token+"/callback", r, nil)
	took := time.Since(started)

	switch {
	case err != nil:
		c.Log.Warn("interaction response failed; Discord will tell the user the app did not respond",
			"took", took.Round(time.Millisecond), "err", err)
	case took > 2*time.Second:
		// Discord allows three seconds. Anything close to that is a warning
		// sign rather than a failure, and worth seeing before it becomes one.
		c.Log.Warn("interaction response was slow", "took", took.Round(time.Millisecond))
	default:
		c.Log.Debug("interaction answered", "took", took.Round(time.Millisecond))
	}
	return err
}

// EditInteractionResponse replaces a deferred reply with the real answer.
func (c *Client) EditInteractionResponse(ctx context.Context, appID, token, content string) error {
	body := map[string]any{"content": content, "allowed_mentions": allowedMentions{Parse: []string{}}}
	return c.do(ctx, http.MethodPatch, "/webhooks/"+appID+"/"+token+"/messages/@original", body, nil)
}

// FollowUp posts an extra message on an interaction, for answers that exceed
// one message or arrive later.
func (c *Client) FollowUp(ctx context.Context, appID, token, content string, ephemeral bool) error {
	body := map[string]any{"content": content, "allowed_mentions": allowedMentions{Parse: []string{}}}
	if ephemeral {
		body["flags"] = MessageFlagEphemeral
	}
	return c.do(ctx, http.MethodPost, "/webhooks/"+appID+"/"+token, body, nil)
}
