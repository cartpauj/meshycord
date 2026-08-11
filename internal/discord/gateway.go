package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// DefaultGatewayURL is the fallback when /gateway/bot cannot be reached.
const DefaultGatewayURL = "wss://gateway.discord.gg"

// gatewayQuery pins the API version and encoding.
//
// No zlib-stream: the payloads here are small, and on an ARMv6 Pi the CPU
// spent decompressing is worth more than the bandwidth saved on a link that
// carries a handful of messages a minute.
const gatewayQuery = "?v=10&encoding=json"

// Handlers is what the bridge cares about. Any nil handler is skipped.
//
// Every one of these except MessageCreate was impossible on the ESP32: they
// are Gateway-only events with no REST endpoint to poll.
type Handlers struct {
	// Ready fires on a fresh connection, carrying the bot's own identity.
	Ready func(botUser User, applicationID string)
	// Resumed fires when a dropped connection was picked back up with no
	// events missed.
	Resumed func()
	// Disconnected fires whenever the link drops, so the UI can say so.
	Disconnected func(err error)

	MessageCreate func(*Message)
	MessageDelete func(channelID, messageID string)
	// ReactionAdd and ReactionRemove are the headline capability. There is no
	// REST endpoint to poll for these; the Gateway is the only way to see one.
	ReactionAdd    func(*ReactionEvent)
	ReactionRemove func(*ReactionEvent)
	ChannelDelete  func(*Channel)
	// InteractionCreate carries slash commands, button presses and modal
	// submissions — the mechanism that lets a room-server password be typed
	// somewhere other than channel history.
	InteractionCreate func(*Interaction)
}

// Gateway is a self-healing Discord Gateway connection.
type Gateway struct {
	Token    func() string
	Handlers Handlers
	Log      *slog.Logger
	// REST is used to look up the gateway URL, which also reports how many
	// session starts remain today.
	REST *Client

	mu        sync.Mutex
	sessionID string
	resumeURL string
	seq       int

	connected atomic.Bool
	upSince   atomic.Int64
	lastErr   atomic.Value // string
	// fatal latches an error that retrying cannot fix — a bad token, or the
	// Message Content intent not enabled in the Developer Portal. Retrying
	// those in a loop achieves nothing except hammering Discord.
	fatal atomic.Value // string

	lastIdentify time.Time
}

// NewGateway builds a Gateway client.
func NewGateway(token func() string, rest *Client, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{Token: token, REST: rest, Log: log}
}

// Connected reports whether the link is currently up.
func (g *Gateway) Connected() bool { return g.connected.Load() }

// UpSince is when the current connection was established.
func (g *Gateway) UpSince() time.Time {
	ms := g.upSince.Load()
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// LastError is the most recent connection failure, for the status page.
func (g *Gateway) LastError() string {
	if s, ok := g.lastErr.Load().(string); ok {
		return s
	}
	return ""
}

// FatalError returns a problem that reconnecting cannot fix, if any. The web
// UI shows this prominently — it always means a setting is wrong.
func (g *Gateway) FatalError() string {
	if s, ok := g.fatal.Load().(string); ok {
		return s
	}
	return ""
}

// Run holds the Gateway connection open until ctx is cancelled.
//
// This is the single largest piece of genuinely fiddly protocol work in the
// project, and where bugs would live. It reconnects forever, resuming when it
// can and re-identifying when it cannot.
func (g *Gateway) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if msg := g.FatalError(); msg != "" {
			// Something a reconnect cannot fix. Check again slowly in case the
			// operator fixes the setting, but do not hammer Discord.
			select {
			case <-ctx.Done():
				return
			case <-time.After(60 * time.Second):
			}
			// Clear it so a corrected token or intent is picked up.
			g.fatal.Store("")
			continue
		}

		err := g.runOnce(ctx)
		g.connected.Store(false)
		g.upSince.Store(0)
		if g.Handlers.Disconnected != nil {
			g.Handlers.Disconnected(err)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			g.lastErr.Store(err.Error())
			g.Log.Warn("gateway disconnected", "err", err, "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(backoff)):
		}
		backoff *= 2
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
	}
}

// runOnce holds one connection for its whole life.
func (g *Gateway) runOnce(ctx context.Context) error {
	token := ""
	if g.Token != nil {
		token = g.Token()
	}
	if token == "" {
		return errors.New("no bot token configured")
	}

	g.mu.Lock()
	resumeURL, sessionID, seq := g.resumeURL, g.sessionID, g.seq
	g.mu.Unlock()

	canResume := resumeURL != "" && sessionID != ""
	dialURL := resumeURL
	if !canResume {
		dialURL = g.gatewayURL(ctx)
	}
	if !strings.Contains(dialURL, "?") {
		dialURL += gatewayQuery
	}

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(connCtx, 30*time.Second)
	conn, _, err := websocket.Dial(dialCtx, dialURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"User-Agent": []string{UserAgent}},
	})
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial gateway: %w", err)
	}
	// A GUILD_CREATE for a large server is far bigger than the library's 32 KB
	// default, and exceeding the limit closes the connection with an error
	// that reads like a network fault.
	conn.SetReadLimit(8 << 20)
	defer conn.CloseNow()

	// HELLO must be the first frame. Anything else means we are not talking to
	// the gateway we think we are.
	hello, err := readPayload(connCtx, conn, 30*time.Second)
	if err != nil {
		return fmt.Errorf("waiting for HELLO: %w", err)
	}
	if hello.Op != OpHello {
		return fmt.Errorf("expected HELLO, got opcode %d", hello.Op)
	}
	var h helloData
	if err := json.Unmarshal(hello.D, &h); err != nil {
		return fmt.Errorf("decode HELLO: %w", err)
	}
	interval := time.Duration(h.HeartbeatInterval) * time.Millisecond
	if interval <= 0 {
		interval = 41250 * time.Millisecond
	}

	var writeMu sync.Mutex
	send := func(op int, d any) error {
		payload, err := json.Marshal(map[string]any{"op": op, "d": d})
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, wcancel := context.WithTimeout(connCtx, 15*time.Second)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageText, payload)
	}

	if canResume {
		g.Log.Info("resuming gateway session", "seq", seq)
		if err := send(OpResume, resumeData{Token: token, SessionID: sessionID, Seq: seq}); err != nil {
			return fmt.Errorf("send RESUME: %w", err)
		}
	} else {
		if err := g.throttleIdentify(connCtx); err != nil {
			return err
		}
		g.Log.Info("identifying with the gateway")
		if err := send(OpIdentify, identifyData{
			Token:      token,
			Intents:    Intents,
			Properties: identifyProperties{OS: "linux", Browser: "meshycord", Device: "meshycord"},
		}); err != nil {
			return fmt.Errorf("send IDENTIFY: %w", err)
		}
	}

	// Heartbeat. The first one is deliberately delayed by a random fraction of
	// the interval, as Discord asks, so a fleet of bots restarting together
	// does not beat in lockstep.
	var acked atomic.Bool
	acked.Store(true)
	hbErr := make(chan error, 1)
	go func() {
		first := time.Duration(rand.Float64() * float64(interval))
		select {
		case <-connCtx.Done():
			return
		case <-time.After(first):
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if !acked.Swap(false) {
				// No ACK since the last beat: the connection is a zombie —
				// the socket is open and nothing is coming through it. Drop it
				// and resume rather than sitting there believing we are
				// connected.
				hbErr <- errors.New("no heartbeat ACK; connection is stale")
				return
			}
			g.mu.Lock()
			s := g.seq
			g.mu.Unlock()
			var d any
			if s > 0 {
				d = s
			}
			if err := send(OpHeartbeat, d); err != nil {
				hbErr <- fmt.Errorf("send heartbeat: %w", err)
				return
			}
			select {
			case <-connCtx.Done():
				return
			case <-t.C:
			}
		}
	}()

	for {
		select {
		case err := <-hbErr:
			return err
		default:
		}

		p, err := readPayload(connCtx, conn, interval+30*time.Second)
		if err != nil {
			// A close code tells us whether retrying is worth anything.
			if fatal := fatalCloseReason(err); fatal != "" {
				g.fatal.Store(fatal)
				g.Log.Error("gateway refused the connection", "reason", fatal)
				g.forgetSession()
				return errors.New(fatal)
			}
			select {
			case herr := <-hbErr:
				return herr
			default:
			}
			return err
		}

		if p.S != nil {
			g.mu.Lock()
			g.seq = *p.S
			g.mu.Unlock()
		}

		switch p.Op {
		case OpHeartbeatACK:
			acked.Store(true)

		case OpHeartbeat:
			// Discord may ask for an immediate beat.
			g.mu.Lock()
			s := g.seq
			g.mu.Unlock()
			if err := send(OpHeartbeat, s); err != nil {
				return err
			}

		case OpReconnect:
			// A polite "please reconnect and resume". Not an error.
			g.Log.Info("gateway asked us to reconnect")
			return nil

		case OpInvalidSession:
			var resumable bool
			_ = json.Unmarshal(p.D, &resumable)
			if !resumable {
				g.forgetSession()
			}
			g.Log.Info("gateway invalidated the session", "resumable", resumable)
			// Discord asks for a 1-5 second pause before identifying again.
			select {
			case <-connCtx.Done():
			case <-time.After(time.Duration(1000+rand.Intn(4000)) * time.Millisecond):
			}
			return nil

		case OpDispatch:
			g.dispatch(p)
		}
	}
}

// throttleIdentify enforces Discord's identify rate limit: one every five
// seconds. Exceeding it invalidates the token's session budget for the day,
// which is a far worse outcome than waiting.
func (g *Gateway) throttleIdentify(ctx context.Context) error {
	g.mu.Lock()
	wait := 5*time.Second - time.Since(g.lastIdentify)
	g.lastIdentify = time.Now()
	g.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

func (g *Gateway) forgetSession() {
	g.mu.Lock()
	g.sessionID, g.resumeURL, g.seq = "", "", 0
	g.mu.Unlock()
}

func (g *Gateway) dispatch(p gatewayPayload) {
	switch p.T {
	case "READY":
		var d readyData
		if err := json.Unmarshal(p.D, &d); err != nil {
			g.Log.Warn("decode READY", "err", err)
			return
		}
		g.mu.Lock()
		g.sessionID = d.SessionID
		g.resumeURL = d.ResumeGatewayURL
		g.mu.Unlock()
		g.connected.Store(true)
		g.upSince.Store(time.Now().UnixMilli())
		g.lastErr.Store("")
		g.Log.Info("gateway ready", "bot", d.User.Username, "session", d.SessionID)
		if g.Handlers.Ready != nil {
			g.Handlers.Ready(d.User, d.Application.ID)
		}

	case "RESUMED":
		g.connected.Store(true)
		g.upSince.Store(time.Now().UnixMilli())
		g.lastErr.Store("")
		g.Log.Info("gateway session resumed with no events missed")
		if g.Handlers.Resumed != nil {
			g.Handlers.Resumed()
		}

	case "MESSAGE_CREATE":
		if g.Handlers.MessageCreate == nil {
			return
		}
		var m Message
		if err := json.Unmarshal(p.D, &m); err != nil {
			g.Log.Warn("decode MESSAGE_CREATE", "err", err)
			return
		}
		g.Handlers.MessageCreate(&m)

	case "MESSAGE_DELETE":
		if g.Handlers.MessageDelete == nil {
			return
		}
		var d struct {
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
		}
		if err := json.Unmarshal(p.D, &d); err == nil {
			g.Handlers.MessageDelete(d.ChannelID, d.ID)
		}

	case "MESSAGE_REACTION_ADD", "MESSAGE_REACTION_REMOVE":
		var e ReactionEvent
		if err := json.Unmarshal(p.D, &e); err != nil {
			g.Log.Warn("decode reaction", "err", err)
			return
		}
		if p.T == "MESSAGE_REACTION_ADD" {
			e.Added = true
			if g.Handlers.ReactionAdd != nil {
				g.Handlers.ReactionAdd(&e)
			}
			return
		}
		if g.Handlers.ReactionRemove != nil {
			g.Handlers.ReactionRemove(&e)
		}

	case "CHANNEL_DELETE":
		if g.Handlers.ChannelDelete == nil {
			return
		}
		var ch Channel
		if err := json.Unmarshal(p.D, &ch); err == nil {
			g.Handlers.ChannelDelete(&ch)
		}

	case "INTERACTION_CREATE":
		if g.Handlers.InteractionCreate == nil {
			return
		}
		var i Interaction
		if err := json.Unmarshal(p.D, &i); err != nil {
			// Do not drop it. An interaction we fail to answer is three
			// seconds of spinner and then "the application did not respond in
			// time" — the least informative failure Discord has. Go fills in
			// whatever it could parse before the error, so if the id and token
			// survived we can still apologise properly, and the log says what
			// actually broke.
			g.Log.Error("could not fully decode an interaction", "type", i.Type, "err", err)
			if i.ID == "" || i.Token == "" {
				return
			}
			g.Log.Warn("answering the interaction anyway so the user is not left waiting")
		}
		g.Handlers.InteractionCreate(&i)
	}
}

// gatewayURL asks Discord where to connect, falling back to the well-known
// host. The /gateway/bot response also carries the remaining session starts,
// which is worth logging when it gets low.
func (g *Gateway) gatewayURL(ctx context.Context) string {
	if g.REST == nil {
		return DefaultGatewayURL
	}
	var out struct {
		URL               string `json:"url"`
		SessionStartLimit struct {
			Remaining int `json:"remaining"`
			Total     int `json:"total"`
		} `json:"session_start_limit"`
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := g.REST.do(rctx, http.MethodGet, "/gateway/bot", nil, &out); err != nil {
		g.Log.Debug("could not read the gateway URL; using the default", "err", err)
		return DefaultGatewayURL
	}
	if out.SessionStartLimit.Total > 0 && out.SessionStartLimit.Remaining < 20 {
		g.Log.Warn("few gateway session starts left today",
			"remaining", out.SessionStartLimit.Remaining, "total", out.SessionStartLimit.Total)
	}
	if out.URL == "" {
		return DefaultGatewayURL
	}
	return out.URL
}

func readPayload(ctx context.Context, conn *websocket.Conn, timeout time.Duration) (gatewayPayload, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, data, err := conn.Read(rctx)
	if err != nil {
		return gatewayPayload{}, err
	}
	var p gatewayPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return gatewayPayload{}, fmt.Errorf("decode gateway frame: %w", err)
	}
	return p, nil
}

// fatalCloseReason turns a websocket close code into an explanation, for the
// codes where reconnecting is pointless. Each of these means a setting is
// wrong, and saying which one saves a long debugging session.
func fatalCloseReason(err error) string {
	status := websocket.CloseStatus(err)
	switch status {
	case 4004:
		return "Discord rejected the bot token. Check it in Settings — it may have been regenerated."
	case 4013:
		return "Discord rejected the requested Gateway intents. This is a bug in MeshyCord; please report it."
	case 4014:
		return "Discord refused a privileged intent. Turn on MESSAGE CONTENT INTENT for the bot in the Discord Developer Portal (Bot → Privileged Gateway Intents), then restart."
	case 4010, 4011:
		return "Discord asked for sharding, which MeshyCord does not implement. The bot is in too many servers."
	}
	return ""
}

// jitter spreads reconnects out so a restarting fleet does not stampede.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int63n(int64(d)))
}
