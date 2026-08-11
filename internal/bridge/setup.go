package bridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"meshycord/internal/discord"
	"meshycord/internal/meshcore"
	"meshycord/internal/store"
)

// Category names.
//
// Categories are the ONE thing matched by name, because Discord gives no other
// way to find one. Everything else is matched by id, so renaming a channel can
// never break routing.
//
// Separate categories per kind is not only tidiness: Discord caps a category
// at 50 channels, and this mesh has 46 room servers on it. One category would
// fill up.
const (
	CategoryAdmin    = "MeshyCord"
	CategoryChannels = "Channels"
	CategoryRooms    = "Room Servers"
	CategoryDMs      = "Companion DMs"

	AdminChannelName = "meshycord-admin"
	InboxChannelName = "global-inbox"
)

// categoryFor is which category a link's channel belongs in.
func categoryFor(kind store.RouteKind) string {
	switch kind {
	case store.KindChannel:
		return CategoryChannels
	case store.KindRoom:
		return CategoryRooms
	default:
		return CategoryDMs
	}
}

// category resolves a category by name, creating it if absent. Ids are cached
// because resolving one costs a full channel listing.
func (b *Bridge) category(ctx context.Context, name string) string {
	b.catMu.Lock()
	if id, ok := b.cats[name]; ok && id != "" {
		b.catMu.Unlock()
		return id
	}
	b.catMu.Unlock()

	guild := b.cfg.GuildID()
	if guild == "" {
		return ""
	}
	chans, err := b.rest.GuildChannels(ctx, guild)
	if err != nil {
		b.log.Warn("could not list channels to find a category", "name", name, "err", err)
		return ""
	}
	for _, ch := range chans {
		// Categories keep their given name verbatim — no slug sanitising.
		if ch.Type == discord.ChannelTypeCategory && ch.Name == name {
			b.rememberCategory(name, ch.ID)
			return ch.ID
		}
	}

	created, err := b.rest.CreateChannel(ctx, guild, discord.CreateChannelRequest{
		Name: name, Type: discord.ChannelTypeCategory,
	})
	if err != nil {
		b.log.Warn("could not create a category", "name", name, "err", err)
		return ""
	}
	b.log.Info("created category", "name", name, "id", created.ID)
	b.rememberCategory(name, created.ID)
	return created.ID
}

func (b *Bridge) rememberCategory(name, id string) {
	b.catMu.Lock()
	if b.cats == nil {
		b.cats = map[string]string{}
	}
	b.cats[name] = id
	b.catMu.Unlock()
}

// forgetCategories drops the cached ids so the next use re-finds or recreates
// them.
//
// Needed because a category deleted in Discord leaves a dead parent id behind,
// and every channel creation then fails until the process restarts.
func (b *Bridge) forgetCategories() {
	b.catMu.Lock()
	b.cats = map[string]string{}
	b.catMu.Unlock()
}

// createLinkedChannel makes a Discord channel for a link, retrying once if the
// cached category turns out to be stale.
func (b *Bridge) createLinkedChannel(ctx context.Context, kind store.RouteKind, name, topic, fallback string) (*discord.Channel, error) {
	guild := b.cfg.GuildID()
	if guild == "" {
		return nil, fmt.Errorf("no Discord server configured")
	}
	clean := SanitizeChannelName(name, fallback)

	req := discord.CreateChannelRequest{
		Name:     clean,
		Type:     discord.ChannelTypeText,
		Topic:    topic,
		ParentID: b.category(ctx, categoryFor(kind)),
	}
	ch, err := b.rest.CreateChannel(ctx, guild, req)
	if err == nil {
		return ch, nil
	}

	// Most likely the cached category was deleted in Discord, leaving a dead
	// parent id. Forget them and try once more; asking again recreates it.
	b.log.Info("channel creation failed; refreshing categories and retrying", "err", err)
	b.forgetCategories()
	req.ParentID = b.category(ctx, categoryFor(kind))
	return b.rest.CreateChannel(ctx, guild, req)
}

// Bootstrap builds the Discord side: categories, #meshycord-admin, the inbox,
// and a check that every stored link still points at a real channel.
//
// Idempotent, and safe to run at any time — it also runs when a channel is
// found to have been deleted.
func (b *Bridge) Bootstrap(ctx context.Context) error {
	if b.cfg.GuildID() == "" {
		return fmt.Errorf("no Discord server (guild) id set — enter it in the web UI")
	}

	// Re-resolve categories from scratch. Bootstrap also runs at runtime when
	// a channel is found deleted, and by then a cached category id may be
	// stale because that was deleted too.
	b.forgetCategories()

	// Verify stored ids before trusting them. A channel deleted by hand would
	// otherwise never come back, because the create step is skipped whenever
	// an id is on file.
	if id := b.cfg.AdminChannel(); id != "" && !b.rest.ChannelExists(ctx, id) {
		b.log.Info("the stored admin channel is gone; recreating")
		_ = b.cfg.SetAdminChannel("")
	}
	if id := b.cfg.InboxChannel(); id != "" && !b.rest.ChannelExists(ctx, id) {
		b.log.Info("the stored inbox channel is gone; recreating")
		_ = b.cfg.SetInboxChannel("")
	}

	// Categories first, so the server has its shape before any channel lands
	// in it.
	adminCat := b.category(ctx, CategoryAdmin)
	b.category(ctx, CategoryChannels)
	b.category(ctx, CategoryRooms)
	b.category(ctx, CategoryDMs)

	if err := b.ensureAdminChannel(ctx, adminCat); err != nil {
		return err
	}

	// The inbox is ours to create rather than something the user has to
	// repurpose. NOTE: only ever move a channel WE created — an earlier
	// version moved one unconditionally and dragged a repurposed #general into
	// its own category.
	if b.cfg.InboxChannel() == "" {
		ch, err := b.rest.CreateChannel(ctx, b.cfg.GuildID(), discord.CreateChannelRequest{
			Name:     InboxChannelName,
			Type:     discord.ChannelTypeText,
			Topic:    "Mesh traffic from senders that have no channel yet. Link one with /mesh link.",
			ParentID: adminCat,
		})
		if err != nil {
			return fmt.Errorf("create the inbox channel: %w", err)
		}
		if err := b.cfg.SetInboxChannel(ch.ID); err != nil {
			return err
		}
		b.log.Info("created the inbox channel", "id", ch.ID)
	}

	// Drop links whose Discord channel no longer exists. Otherwise a deleted
	// channel keeps its link, auto-linking skips it as "already linked", and
	// it never recovers.
	routes, err := b.db.Routes()
	if err != nil {
		return err
	}
	for _, r := range routes {
		if b.rest.ChannelExists(ctx, r.ChannelID) {
			continue
		}
		b.log.Info("link points at a channel that no longer exists; removing",
			"kind", r.Kind, "key", r.MeshKey, "channel", r.ChannelID)
		if _, err := b.db.DeleteRoute(r.Kind, r.MeshKey); err != nil {
			b.log.Warn("could not remove a dead link", "err", err)
		}
	}

	b.registerCommands(ctx)
	return nil
}

// ensureAdminChannel finds or creates #meshycord-admin.
func (b *Bridge) ensureAdminChannel(ctx context.Context, categoryID string) error {
	if b.cfg.AdminChannel() != "" {
		return nil
	}
	// Adopt an existing #meshycord-admin by name if there is one, but leave it
	// where the user put it — only channels we create get placed in a category.
	if chans, err := b.rest.GuildChannels(ctx, b.cfg.GuildID()); err == nil {
		want := SanitizeChannelName(AdminChannelName, "")
		for _, ch := range chans {
			if ch.Type == discord.ChannelTypeText && ch.Name == want {
				b.log.Info("adopting the existing admin channel", "id", ch.ID)
				return b.cfg.SetAdminChannel(ch.ID)
			}
		}
	}
	ch, err := b.rest.CreateChannel(ctx, b.cfg.GuildID(), discord.CreateChannelRequest{
		Name:     AdminChannelName,
		Type:     discord.ChannelTypeText,
		Topic:    "Type commands here, or use /mesh. `help` for the list.",
		ParentID: categoryID,
	})
	if err != nil {
		return fmt.Errorf("create #%s: %w", AdminChannelName, err)
	}
	b.log.Info("created the admin channel", "id", ch.ID)
	return b.cfg.SetAdminChannel(ch.ID)
}

// syncAfterMesh runs once the node is reachable and channel names are known.
//
// Mesh channels auto-link if that policy is on. Room servers are deliberately
// NOT bulk-created: there are often dozens, which would blow Discord's
// per-category limit and its channel-creation rate limit.
//
// Both halves have to be up before this can do anything — it needs channel
// names from the radio and a place to create channels in Discord — and they
// come up in whichever order they please. So it is called from BOTH sides,
// guarded rather than sequenced: whichever finishes second is the one that
// actually does the work. Calling it from only the Discord side meant that a
// radio plugged in after startup never got its channels linked at all.
func (b *Bridge) syncAfterMesh(ctx context.Context) {
	sess := b.link.Session()
	if sess == nil {
		return
	}
	if !b.ready.Load() {
		return // Discord has no channels to create in yet; setup() will call us
	}
	if b.syncing.Swap(true) {
		return // already running for the other half
	}
	defer b.syncing.Store(false)

	// Drop channel links whose slot does not exist on the node. A phantom link
	// can be left by a mis-attributed CMD_GET_CHANNEL reply — a timed-out
	// query for slot N answered during the query for slot N+1 — which produced
	// a duplicate Discord channel for a slot that was never real.
	routes, err := b.db.Routes()
	if err != nil {
		b.log.Warn("could not read links", "err", err)
		return
	}
	for _, r := range routes {
		if r.Kind != store.KindChannel {
			continue
		}
		idx, ok := parseChannelSlot(r.MeshKey)
		if !ok {
			continue
		}
		if _, live := sess.Channel(idx); !live {
			b.log.Info("channel slot is not a real channel; removing the phantom link",
				"slot", idx, "discord_channel", r.ChannelID)
			_, _ = b.db.DeleteRoute(store.KindChannel, r.MeshKey)
		}
	}

	if !b.cfg.AutoCreateChannels() {
		return
	}

	created := 0
	for _, ci := range sess.Channels() {
		key := fmt.Sprintf("%d", ci.Index)
		if _, err := b.db.Route(store.KindChannel, key); err == nil {
			continue
		}
		ch, err := b.createLinkedChannel(ctx, store.KindChannel, ci.Name,
			fmt.Sprintf("MeshCore channel %d (%s)", ci.Index, ci.Name),
			"mesh-channel-"+key)
		if err != nil {
			b.log.Warn("could not auto-link a mesh channel", "slot", ci.Index, "err", err)
			continue
		}
		if _, err := b.db.PutRoute(store.KindChannel, key, ch.ID, ci.Name); err != nil {
			b.log.Warn("could not save an auto-created link", "err", err)
			continue
		}
		created++
		b.log.Info("auto-linked a mesh channel", "slot", ci.Index, "name", ci.Name)
		b.db.LogEvent("info", "bridge", fmt.Sprintf("auto-linked mesh channel %d (%s)", ci.Index, ci.Name))
		// Be gentle with Discord's channel-creation limit, which is far
		// stricter than the ordinary message rate limit.
		select {
		case <-ctx.Done():
			return
		case <-time.After(1200 * time.Millisecond):
		}
	}
	if created > 0 {
		b.adminSay(ctx, fmt.Sprintf(
			"Auto-linked %d mesh channel(s). Room servers are not bulk-created — there are often "+
				"dozens. Use `list rooms unlinked` then `add <n>`, or `sync rooms` to link them all.",
			created))
	}
}

// Reset deletes everything the bridge created and clears every link.
//
// Deliberately narrow: the admin channel and the MeshyCord category are kept
// so there is somewhere to report back to, and nothing the user made by hand
// is ever touched.
func (b *Bridge) Reset(ctx context.Context) string {
	routes, err := b.db.Routes()
	if err != nil {
		return "Could not read the links: " + err.Error()
	}

	deleted, forgotten := 0, 0
	for _, r := range routes {
		if err := b.rest.DeleteChannel(ctx, r.ChannelID); err == nil {
			deleted++
		}
		// A reset is meant to leave nothing behind, and leaving stored secrets
		// for links that no longer exist is not that.
		if r.Kind == store.KindRoom && b.db.HasRoomPassword(r.MeshKey) {
			_ = b.db.SetRoomPassword(r.MeshKey, "")
			forgotten++
		}
	}
	_ = b.db.ClearRoutes()

	if id := b.cfg.InboxChannel(); id != "" {
		if err := b.rest.DeleteChannel(ctx, id); err == nil {
			deleted++
		}
		_ = b.cfg.SetInboxChannel("")
	}

	for _, name := range []string{CategoryChannels, CategoryRooms, CategoryDMs} {
		if id := b.category(ctx, name); id != "" {
			if err := b.rest.DeleteChannel(ctx, id); err == nil {
				deleted++
			}
		}
	}
	b.forgetCategories()
	b.clearSnapshots()

	// Recreate a clean inbox so the bridge stays usable.
	if err := b.Bootstrap(ctx); err != nil {
		return fmt.Sprintf("Deleted %d channel(s), but rebuilding failed: %v", deleted, err)
	}

	msg := fmt.Sprintf("Reset done — deleted %d channel(s)/category(ies) and cleared every link.", deleted)
	if forgotten > 0 {
		msg += fmt.Sprintf(" Forgot %d stored room password(s).", forgotten)
	}
	msg += " A fresh **#" + InboxChannelName + "** has been created."
	b.db.LogEvent("warn", "admin", msg)
	return msg
}

// Rediscover forgets every Discord-side id and builds it all again, which
// reproduces exactly what a new user sees on first connect.
func (b *Bridge) Rediscover(ctx context.Context) string {
	b.log.Info("rediscover: clearing Discord-side state")
	_ = b.cfg.SetAdminChannel("")
	_ = b.cfg.SetInboxChannel("")
	_ = b.db.ClearRoutes()
	b.forgetCategories()

	if err := b.Bootstrap(ctx); err != nil {
		return "Bootstrap failed: " + err.Error() +
			"\nCheck the bot token, and that the server ID is right."
	}
	b.syncAfterMesh(ctx)

	routes, _ := b.db.Routes()
	return fmt.Sprintf("Rediscovered.\nserver : %s\nadmin  : %s\ninbox  : %s\nlinks  : %d",
		b.cfg.GuildID(), b.cfg.AdminChannel(), b.cfg.InboxChannel(), len(routes))
}

func parseChannelSlot(key string) (byte, bool) {
	n := 0
	if key == "" {
		return 0, false
	}
	for _, r := range key {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 255 {
			return 0, false
		}
	}
	return byte(n), true
}

// adminSay posts to #meshycord-admin, truncating to Discord's message ceiling.
func (b *Bridge) adminSay(ctx context.Context, text string) {
	ch := b.cfg.AdminChannel()
	if ch == "" || strings.TrimSpace(text) == "" {
		return
	}
	if _, err := b.rest.SendMessage(ctx, ch, clampReply(text)); err != nil {
		b.log.Warn("could not post to the admin channel", "err", err)
	}
}

// maxReplyLen keeps replies inside Discord's 2000-character message limit,
// with room for the truncation notice.
const maxReplyLen = 1900

func clampReply(s string) string {
	if len(s) <= maxReplyLen {
		return s
	}
	return meshcore.TruncateUTF8(s, maxReplyLen) + "\n…truncated"
}
