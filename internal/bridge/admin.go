package bridge

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"meshycord/internal/discord"
	"meshycord/internal/meshcore"
	"meshycord/internal/store"
)

// The admin console.
//
// Everything the bridge can be told to do is reachable by typing in
// #meshycord-admin, and the same executor backs the /mesh slash commands — so
// the two can never drift apart. Typed commands matter because the web UI is
// not reachable from outside the house, and the radio is.

// Listing numbering is frozen for ten minutes, so `add 7` always means the row
// you saw. Contacts adverting in afterwards can never shift what 7 refers to.
const (
	snapshotTTL = 10 * time.Minute
	maxListRows = 25
	maxSnapshot = 200
)

type listRow struct {
	Kind       store.RouteKind
	Key        string // key prefix, or channel slot
	Label      string
	PubKey     string // full key, when known — `contact remove` needs it
	Type       int
	Hops       int
	LastAdvert time.Time
	Linked     bool
}

type listSnapshot struct {
	at   time.Time
	rows []listRow
	// contacts marks a snapshot produced by `contact find`, whose rows carry
	// full public keys and address the node rather than a link.
	contacts bool
}

func (b *Bridge) putSnapshot(actor string, rows []listRow, contacts bool) {
	if len(rows) > maxSnapshot {
		rows = rows[:maxSnapshot]
	}
	b.snapMu.Lock()
	b.snapshots[actor] = &listSnapshot{at: time.Now(), rows: rows, contacts: contacts}
	b.snapMu.Unlock()
}

func (b *Bridge) getSnapshot(actor string) (*listSnapshot, bool) {
	b.snapMu.Lock()
	defer b.snapMu.Unlock()
	s, ok := b.snapshots[actor]
	if !ok || time.Since(s.at) > snapshotTTL {
		return nil, false
	}
	return s, true
}

func (b *Bridge) clearSnapshots() {
	b.snapMu.Lock()
	b.snapshots = map[string]*listSnapshot{}
	b.snapMu.Unlock()
}

// handleAdminMessage runs a typed command and posts the answer.
func (b *Bridge) handleAdminMessage(ctx context.Context, m *discord.Message) {
	reply := b.Exec(ctx, m.Author.ID, m.Content, m.ChannelID, m.ID)
	if reply != "" {
		b.say(ctx, m.ChannelID, reply)
	}
}

// Exec runs one admin command and returns the reply.
//
// actor scopes the frozen listing numbering, so two people listing at once do
// not renumber each other's rows. messageID lets a command delete its own
// Discord message, which `login` does so a typed password does not linger.
func (b *Bridge) Exec(ctx context.Context, actor, line, channelID, messageID string) string {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)

	switch {
	case lower == "help" || lower == "?":
		return b.cmdHelp()
	case lower == "status":
		return b.cmdStatus()
	case lower == "reset":
		return "This deletes every channel the bridge created — all linked channels, the " +
			"**Channels / Room Servers / Companion DMs** categories, and **#" + InboxChannelName +
			"** — and clears every link.\nChannels you made yourself are untouched.\n\n" +
			"Send `reset confirm` to go ahead."
	case lower == "reset confirm":
		return b.Reset(ctx)
	case lower == "tidy":
		return b.cmdTidy(ctx)
	case lower == "sync rooms":
		return b.cmdSyncRooms(ctx, false)
	case lower == "sync rooms confirm":
		return b.cmdSyncRooms(ctx, true)
	case lower == "rediscover":
		return b.Rediscover(ctx)
	}

	if rest, ok := cutPrefix(raw, "contact "); ok {
		return b.cmdContact(ctx, actor, rest)
	}
	if rest, ok := cutPrefix(raw, "login"); ok {
		return b.cmdLogin(ctx, actor, strings.TrimSpace(rest), channelID, messageID)
	}
	if rest, ok := cutPrefix(raw, "add "); ok {
		return b.cmdAdd(ctx, actor, rest)
	}
	if rest, ok := cutPrefix(raw, "remove "); ok {
		return b.cmdRemove(ctx, actor, rest)
	}
	if rest, ok := cutPrefix(raw, "rm "); ok {
		return b.cmdRemove(ctx, actor, rest)
	}
	if rest, ok := cutPrefix(raw, "find "); ok {
		return b.cmdList(actor, "find", rest)
	}
	if rest, ok := cutPrefix(raw, "search "); ok {
		return b.cmdList(actor, "find", rest)
	}
	if strings.HasPrefix(lower, "list") {
		return b.cmdList(actor, "list", strings.TrimSpace(raw[4:]))
	}

	return "Unknown command. Try `help`, or use `/mesh`."
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// ---------------------------------------------------------------------------
// help and status
// ---------------------------------------------------------------------------

func (b *Bridge) cmdHelp() string {
	return "**MeshyCord commands**\n```\n" +
		"list rooms | companions | channels | links | repeaters | sensors\n" +
		"find <text>\n" +
		"   modifiers: unlinked, recent, name, hops, desc\n" +
		"   e.g. list rooms unlinked hops\n" +
		"        find ridge name\n" +
		"add <n>              link item n from the last listing\n" +
		"add <keyprefix>      link by 12-char key, even if not a contact\n" +
		"add <n> as <name>    choose the Discord channel name\n" +
		"remove <n|keyprefix> unlink\n" +
		"tidy                 drop links whose channel or slot is gone\n" +
		"\n" +
		"contact add <key> <type> [+link] <name>  add a node; +link makes its channel\n" +
		"     type: room | companion | repeater | sensor  (required)\n" +
		"contact find <text>               search EVERY contact on the node\n" +
		"contact list                      ...all of them\n" +
		"contact info <n|keyprefix>        full details, incl. the 64-hex key\n" +
		"contact rename <n|key> <name>     rename it on the node\n" +
		"contact type <n|key> room|companion   fix what it IS\n" +
		"contact reset-path <n|key>        forget its stored route\n" +
		"contact refresh                   re-read the list from the node\n" +
		"contact remove <n|key> [+delete]  delete it; +delete drops its channel\n" +
		"\n" +
		"login <n|keyprefix> <password>    log in to a room server\n" +
		"login <n|keyprefix>               forget its stored password\n" +
		"\n" +
		"status\n" +
		"sync rooms           link every known room server (asks first)\n" +
		"reset                delete everything the bridge created\n" +
		"help\n" +
		"```\n" +
		"**Prefer `/mesh login`** — a private popup, so the password never enters channel " +
		"history. Typing `login` here works too; that message is deleted as soon as it is read.\n" +
		"_In a linked channel: `path:flood <text>` clears the stored route first; react " +
		EmojiRetry + " **on the failed message itself** to send it again — that clears the " +
		"stored route too, so the retry floods instead of repeating down a route that just " +
		"failed; `!promote <keyprefix>` gives a sender their own channel._\n" +
		"_Listings freeze numbering for 10 minutes, so `add 7` always means the 7 you saw. " +
		"Routing is by key, so renaming a Discord channel never breaks anything._"
}

func (b *Bridge) cmdStatus() string {
	st := b.Status("")
	var s strings.Builder
	s.WriteString("**Status**\n```\n")

	link := "DOWN"
	if st.Mesh.Connected {
		link = "connected"
	}
	fmt.Fprintf(&s, "mesh link   : %s (%s)\n", link, st.Mesh.Transport)
	if st.Mesh.NodeName != "" {
		fmt.Fprintf(&s, "node        : %s\n", st.Mesh.NodeName)
	}
	if !st.Mesh.Connected && st.Mesh.LastError != "" {
		fmt.Fprintf(&s, "last error  : %s\n", meshcore.TruncateUTF8(st.Mesh.LastError, 60))
	}
	discordState := "DOWN"
	if st.DiscordUp {
		discordState = "connected"
	}
	fmt.Fprintf(&s, "discord     : %s\n", discordState)
	fmt.Fprintf(&s, "contacts    : %d cached\n", st.Mesh.Contacts)
	fmt.Fprintf(&s, "mesh chans  : %d\n", st.Mesh.Channels)
	fmt.Fprintf(&s, "links       : %d\n", st.Links)
	fmt.Fprintf(&s, "rooms       : %d logged in of %d\n", st.RoomsLoggedIn, st.RoomsKnown)
	fmt.Fprintf(&s, "awaiting ack: %d\n", st.PendingDelivery)
	fmt.Fprintf(&s, "history     : %d messages (%d in, %d out)\n",
		st.Stats.Messages, st.Stats.MessagesIn, st.Stats.MessagesOut)
	fmt.Fprintf(&s, "last 24h    : %d\n", st.Stats.Last24h)
	fmt.Fprintf(&s, "uptime      : %s\n", roundDuration(st.Uptime))
	s.WriteString("```")

	if st.DiscordFatal != "" {
		s.WriteString("\n⚠ " + st.DiscordFatal)
	}
	return s.String()
}

func roundDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	case d < 48*time.Hour:
		return d.Round(time.Minute).String()
	default:
		return fmt.Sprintf("%dd %s", int(d.Hours())/24, (d % (24 * time.Hour)).Round(time.Hour))
	}
}

// ---------------------------------------------------------------------------
// Listings
// ---------------------------------------------------------------------------

func (b *Bridge) cmdList(actor, mode, args string) string {
	lower := strings.ToLower(args)

	// Modifiers may appear anywhere in the line.
	unlinkedOnly := strings.Contains(lower, "unlinked")
	desc := strings.Contains(lower, "desc")
	sortKey := "recent"
	if strings.Contains(lower, "name") {
		sortKey = "name"
	}
	if strings.Contains(lower, "hops") {
		sortKey = "hops"
	}

	var (
		rows  []listRow
		title string
	)

	if mode == "list" {
		switch {
		case strings.Contains(lower, "room"):
			rows, title = b.contactRows(meshcore.AdvTypeRoom, unlinkedOnly, ""), "Room servers"
		case strings.Contains(lower, "compan"):
			rows, title = b.contactRows(meshcore.AdvTypeChat, unlinkedOnly, ""), "Companions"
		case strings.Contains(lower, "repeat"):
			rows, title = b.contactRows(meshcore.AdvTypeRepeater, false, ""), "Repeaters"
		case strings.Contains(lower, "sensor"):
			rows, title = b.contactRows(meshcore.AdvTypeSensor, false, ""), "Sensors"
		case strings.Contains(lower, "chan"):
			rows, title = b.channelRows(unlinkedOnly, ""), "Mesh channels"
		case strings.Contains(lower, "link"):
			rows, title = b.linkRows(""), "Links"
		default:
			return "`list rooms`, `list companions`, `list channels`, `list links`, " +
				"`list repeaters` or `list sensors`."
		}
	} else {
		// A search. Strip trailing modifiers off the search text so
		// `find ridge name` searches for "ridge", not "ridge name".
		filter := args
		for _, mod := range []string{"unlinked", "recent", "name", "hops", "desc"} {
			l := strings.ToLower(filter)
			if strings.HasSuffix(l, mod) {
				filter = strings.TrimSpace(filter[:len(filter)-len(mod)])
			}
		}
		filter = strings.TrimSpace(filter)
		if filter == "" {
			return "Usage: `find <text>`"
		}
		rows = b.contactRows(-1, unlinkedOnly, filter)
		rows = append(rows, b.channelRows(unlinkedOnly, filter)...)
		title = fmt.Sprintf("Matching %q", filter)
	}

	if len(rows) == 0 {
		return "Nothing matched."
	}
	sortRows(rows, sortKey, desc)
	b.putSnapshot(actor, rows, false)
	return renderRows(rows, title)
}

// contactRows builds listing rows from the cached contacts. typeFilter of -1
// means every type.
func (b *Bridge) contactRows(typeFilter int, unlinkedOnly bool, filter string) []listRow {
	contacts, err := b.db.Contacts(typeFilter)
	if err != nil {
		return nil
	}
	filter = strings.ToLower(filter)

	var out []listRow
	for _, c := range contacts {
		kind := store.KindDM
		if c.Type == meshcore.AdvTypeRoom {
			kind = store.KindRoom
		}
		_, linkErr := b.db.RouteByPrefix(c.Prefix)
		linked := linkErr == nil
		if unlinkedOnly && linked {
			continue
		}
		label := c.Name
		if label == "" {
			label = c.Prefix
		}
		if filter != "" &&
			!strings.Contains(strings.ToLower(label), filter) &&
			!strings.HasPrefix(c.Prefix, filter) {
			continue
		}
		out = append(out, listRow{
			Kind: kind, Key: c.Prefix, Label: label, PubKey: c.PubKey,
			Type: c.Type, Hops: c.OutPathLen, LastAdvert: c.LastAdvert, Linked: linked,
		})
	}
	return out
}

func (b *Bridge) channelRows(unlinkedOnly bool, filter string) []listRow {
	sess := b.link.Session()
	if sess == nil {
		return nil
	}
	filter = strings.ToLower(filter)
	var out []listRow
	for _, ci := range sess.Channels() {
		key := strconv.Itoa(int(ci.Index))
		_, err := b.db.Route(store.KindChannel, key)
		linked := err == nil
		if unlinkedOnly && linked {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(ci.Name), filter) {
			continue
		}
		out = append(out, listRow{
			Kind: store.KindChannel, Key: key, Label: ci.Name, Hops: 0xFF, Linked: linked,
		})
	}
	return out
}

func (b *Bridge) linkRows(filter string) []listRow {
	routes, err := b.db.Routes()
	if err != nil {
		return nil
	}
	filter = strings.ToLower(filter)
	var out []listRow
	for _, r := range routes {
		label := r.Label
		if label == "" {
			label = r.MeshKey
		}
		if filter != "" && !strings.Contains(strings.ToLower(label), filter) {
			continue
		}
		out = append(out, listRow{
			Kind: r.Kind, Key: r.MeshKey, Label: label, Hops: 0xFF,
			LastAdvert: r.LastActivity, Linked: true,
		})
	}
	return out
}

func sortRows(rows []listRow, key string, desc bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, c := rows[i], rows[j]
		var less bool
		switch key {
		case "name":
			less = strings.ToLower(a.Label) < strings.ToLower(c.Label)
		case "hops":
			// Compare decoded hop counts: the stored byte is packed, so a
			// one-hop path recorded with two-byte hashes (65) would otherwise
			// sort after a five-hop one. Unknown paths sort last.
			less = HopCount(a.Hops) < HopCount(c.Hops)
		default: // recent: newest first
			less = a.LastAdvert.After(c.LastAdvert)
		}
		if desc {
			return !less
		}
		return less
	})
}

func renderRows(rows []listRow, title string) string {
	var s strings.Builder
	fmt.Fprintf(&s, "**%s** — %d item(s)\n```\n", title, len(rows))

	shown := 0
	for i, r := range rows {
		if shown >= maxListRows {
			break
		}
		shown++
		mark := "[ ]"
		if r.Linked {
			mark = "[x]"
		}
		// Trim on a UTF-8 boundary: cutting at a byte would slice an emoji in
		// half and put invalid bytes into the Discord message.
		name := meshcore.TruncateUTF8(r.Label, 24)
		fmt.Fprintf(&s, "%2d %s %s", i+1, mark, PadDisplay(name, 26))
		if r.Kind != store.KindChannel {
			s.WriteString(HopsLabel(r.Hops))
		} else {
			s.WriteString("slot " + r.Key)
		}
		s.WriteString("\n")
	}
	s.WriteString("```")
	if len(rows) > shown {
		fmt.Fprintf(&s, "\n_%d more — narrow it with `find <text>`_", len(rows)-shown)
	}
	s.WriteString("\n`[x]` = already linked. Use `add <n>` / `remove <n>`.")
	return s.String()
}

// ---------------------------------------------------------------------------
// Linking
// ---------------------------------------------------------------------------

// isKeyPrefix reports whether a token looks like a key prefix: 8 or 12 hex
// characters.
func isKeyPrefix(t string) (string, bool) {
	x := trimLower(t)
	if len(x) != 8 && len(x) != 12 {
		return "", false
	}
	for i := 0; i < len(x); i++ {
		c := x[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return x, true
}

// resolveTarget turns a token into something to act on: a listing number, or a
// key prefix.
//
// A prefix works even for someone NOT in the node's contact list — a stranger
// whose DM landed in the inbox, say — because routing only needs the key.
func (b *Bridge) resolveTarget(actor, token string) (listRow, error) {
	token = strings.TrimSpace(token)

	if prefix, ok := isKeyPrefix(token); ok {
		row := listRow{Kind: store.KindDM, Key: prefix, Label: prefix, Hops: 0xFF}
		if c, found := b.db.ContactByPrefix(prefix); found {
			if c.Type == meshcore.AdvTypeRoom {
				row.Kind = store.KindRoom
			}
			if c.Name != "" {
				row.Label = c.Name
			}
			row.PubKey, row.Type, row.Hops = c.PubKey, c.Type, c.OutPathLen
		} else {
			b.log.Info("linking a key that is not in the node's contacts", "key", prefix)
		}
		return row, nil
	}

	n, err := strconv.Atoi(token)
	if err != nil || n <= 0 {
		return listRow{}, fmt.Errorf("Give a listing number or a 12-character key prefix.")
	}
	snap, ok := b.getSnapshot(actor)
	if !ok {
		return listRow{}, fmt.Errorf("That listing has expired. Run `list` or `find` again.")
	}
	if n > len(snap.rows) {
		return listRow{}, fmt.Errorf("No item %d in the last listing (1-%d).", n, len(snap.rows))
	}
	return snap.rows[n-1], nil
}

func (b *Bridge) cmdAdd(ctx context.Context, actor, args string) string {
	args = strings.TrimSpace(args)
	custom := ""
	// `add 7 as Ridge Cabin` names the Discord channel; routing still uses the
	// key, so the name is cosmetic.
	if at := strings.Index(strings.ToLower(args), " as "); at > 0 {
		custom = strings.TrimSpace(args[at+4:])
		args = strings.TrimSpace(args[:at])
	}

	row, err := b.resolveTarget(actor, args)
	if err != nil {
		return err.Error()
	}
	return b.linkRow(ctx, row, custom)
}

// CreateLink links a mesh source the caller has already identified exactly.
//
// The web console needs this rather than Exec: there, a mesh channel is
// identified by its slot number, and a bare number typed in the admin console
// means "row N of the last listing" instead. Same work, unambiguous inputs.
func (b *Bridge) CreateLink(ctx context.Context, kind store.RouteKind, key, customName string) string {
	if !kind.Valid() || key == "" {
		return "Nothing to link."
	}
	row := listRow{Kind: kind, Key: key, Label: key, Hops: 0xFF}
	switch kind {
	case store.KindChannel:
		if sess := b.link.Session(); sess != nil {
			if idx, ok := parseChannelSlot(key); ok {
				if ci, live := sess.Channel(idx); live && ci.Name != "" {
					row.Label = ci.Name
				}
			}
		}
	default:
		if c, found := b.db.ContactByPrefix(key); found {
			if c.Name != "" {
				row.Label = c.Name
			}
			row.PubKey, row.Type, row.Hops = c.PubKey, c.Type, c.OutPathLen
		}
	}
	return b.linkRow(ctx, row, customName)
}

// linkRow does the actual work: create a Discord channel and record the link.
func (b *Bridge) linkRow(ctx context.Context, row listRow, custom string) string {
	if _, err := b.db.Route(row.Kind, row.Key); err == nil {
		return "**" + row.Label + "** is already linked."
	}

	name := custom
	if name == "" {
		name = row.Label
		if row.Kind == store.KindChannel {
			name = "mesh-" + row.Label
		}
	}
	var topic, fallback string
	switch row.Kind {
	case store.KindChannel:
		topic = "MeshCore channel " + row.Key
		fallback = "mesh-channel-" + row.Key
	case store.KindRoom:
		topic = "MeshCore room server " + row.Key
		fallback = "node-" + firstN(row.Key, 6)
	default:
		topic = "MeshCore DM " + row.Key
		fallback = "node-" + firstN(row.Key, 6)
	}

	ch, err := b.createLinkedChannel(ctx, row.Kind, name, topic, fallback)
	if err != nil {
		return "Could not create a channel for **" + row.Label + "**: " + err.Error() +
			"\nCheck the bot has Manage Channels, and the 500-channel server limit."
	}
	if _, err := b.db.PutRoute(row.Kind, row.Key, ch.ID, row.Label); err != nil {
		return "Created the channel but could not save the link: " + err.Error()
	}
	b.db.LogEvent("info", "admin", fmt.Sprintf("linked %s %s -> #%s", row.Kind, row.Key, ch.Name))

	msg := "Linked **" + row.Label + "** `" + row.Key + "` -> <#" + ch.ID + ">"
	if row.Kind == store.KindRoom && !b.db.HasRoomPassword(row.Key) {
		msg += "\nRoom servers need a password before they accept posts. " +
			"Run `/mesh login` and type it into the popup — it never enters channel history."
	}
	return msg
}

func (b *Bridge) cmdRemove(ctx context.Context, actor, args string) string {
	row, err := b.resolveTarget(actor, args)
	if err != nil {
		return err.Error()
	}

	// A prefix could be linked as either kind; try both.
	gone, _ := b.db.DeleteRoute(row.Kind, row.Key)
	if !gone && row.Kind == store.KindDM {
		gone, _ = b.db.DeleteRoute(store.KindRoom, row.Key)
	}
	if !gone && row.Kind == store.KindRoom {
		gone, _ = b.db.DeleteRoute(store.KindDM, row.Key)
	}
	if !gone {
		return "**" + row.Label + "** was not linked."
	}

	// Unlinking a room server should not leave its password behind.
	extra := ""
	if b.db.HasRoomPassword(row.Key) {
		_ = b.db.SetRoomPassword(row.Key, "")
		extra = " Its stored room password has been forgotten."
	}
	b.db.LogEvent("info", "admin", fmt.Sprintf("unlinked %s %s", row.Kind, row.Key))
	return "Unlinked **" + row.Label + "** `" + row.Key +
		"`. The Discord channel is left in place — delete it yourself if you want it gone." + extra
}

// cmdTidy drops links that can no longer work: a Discord channel deleted by
// hand, or a mesh channel slot that is not real.
func (b *Bridge) cmdTidy(ctx context.Context) string {
	routes, err := b.db.Routes()
	if err != nil {
		return "Could not read the links: " + err.Error()
	}
	sess := b.link.Session()

	deadChannels, phantomSlots := 0, 0
	for _, r := range routes {
		if !b.rest.ChannelExists(ctx, r.ChannelID) {
			if ok, _ := b.db.DeleteRoute(r.Kind, r.MeshKey); ok {
				deadChannels++
			}
			continue
		}
		if r.Kind != store.KindChannel || sess == nil {
			continue
		}
		idx, ok := parseChannelSlot(r.MeshKey)
		if !ok {
			continue
		}
		if _, live := sess.Channel(idx); !live {
			if ok, _ := b.db.DeleteRoute(r.Kind, r.MeshKey); ok {
				phantomSlots++
			}
		}
	}

	if deadChannels == 0 && phantomSlots == 0 {
		return "Nothing to tidy — every link points at a real Discord channel and a real mesh source."
	}
	msg := "Tidied up."
	if deadChannels > 0 {
		msg += fmt.Sprintf(" Removed %d link(s) whose Discord channel no longer exists.", deadChannels)
	}
	if phantomSlots > 0 {
		msg += fmt.Sprintf(" Removed %d link(s) to channel slots the node does not have "+
			"(their Discord channels are left in place).", phantomSlots)
	}
	if sess == nil {
		msg += " The node is not connected, so mesh channel slots were not checked."
	}
	b.db.LogEvent("info", "admin", msg)
	return msg
}

// cmdSyncRooms links every known room server.
//
// Explicit, never automatic: linking every room server can mean dozens of
// channels, Discord's 50-per-category ceiling, and its strict creation limit.
func (b *Bridge) cmdSyncRooms(ctx context.Context, confirmed bool) string {
	rooms, err := b.db.Contacts(meshcore.AdvTypeRoom)
	if err != nil {
		return "Could not read the contacts: " + err.Error()
	}
	var pending []store.Contact
	for _, c := range rooms {
		if _, err := b.db.Route(store.KindRoom, c.Prefix); err != nil {
			pending = append(pending, c)
		}
	}
	if len(pending) == 0 {
		return "Every known room server is already linked."
	}
	if !confirmed {
		return fmt.Sprintf(
			"This would create **%d** channels in **%s** (Discord caps a category at 50) and takes "+
				"about %ds because of rate limits.\nSend `sync rooms confirm` to proceed.",
			len(pending), CategoryRooms, len(pending)*2)
	}

	created := 0
	for _, c := range pending {
		if created >= 45 { // stay inside Discord's per-category ceiling
			break
		}
		label := c.Name
		if label == "" {
			label = c.Prefix
		}
		ch, err := b.createLinkedChannel(ctx, store.KindRoom, label,
			"MeshCore room server "+c.Prefix, "node-"+firstN(c.Prefix, 6))
		if err != nil {
			b.log.Warn("could not create a room channel", "key", c.Prefix, "err", err)
			continue
		}
		if _, err := b.db.PutRoute(store.KindRoom, c.Prefix, ch.ID, c.Name); err == nil {
			created++
		}
		select {
		case <-ctx.Done():
			return fmt.Sprintf("Stopped early: linked %d room server(s).", created)
		case <-time.After(1500 * time.Millisecond):
		}
	}
	b.db.LogEvent("info", "admin", fmt.Sprintf("bulk-linked %d room servers", created))
	return fmt.Sprintf("Linked %d room server(s).", created)
}

// ---------------------------------------------------------------------------
// Contacts on the node
// ---------------------------------------------------------------------------

func (b *Bridge) cmdContact(ctx context.Context, actor, args string) string {
	args = strings.TrimSpace(args)
	lower := strings.ToLower(args)

	switch {
	case lower == "list" || lower == "find":
		return b.cmdContactFind(actor, "")
	case strings.HasPrefix(lower, "list "):
		return b.cmdContactFind(actor, strings.TrimSpace(args[5:]))
	case strings.HasPrefix(lower, "find "):
		return b.cmdContactFind(actor, strings.TrimSpace(args[5:]))
	case lower == "add":
		return b.contactAddUsage("")
	case strings.HasPrefix(lower, "add "):
		return b.cmdContactAdd(ctx, strings.TrimSpace(args[4:]))
	case lower == "remove" || lower == "rm":
		return "Usage: `contact remove <n|keyprefix> [+delete]`\n" +
			"Uses a number from the last `contact find`, a key prefix, or a full 64-hex key. " +
			"`+delete` also deletes its Discord channel."
	case strings.HasPrefix(lower, "remove "):
		return b.cmdContactRemove(ctx, actor, strings.TrimSpace(args[7:]))
	case strings.HasPrefix(lower, "rm "):
		return b.cmdContactRemove(ctx, actor, strings.TrimSpace(args[3:]))
	case lower == "refresh" || lower == "sync":
		return b.cmdContactRefresh(ctx)
	case lower == "type" || lower == "set-type":
		return "Usage: `contact type <n|keyprefix> room|companion|repeater|sensor`"
	case strings.HasPrefix(lower, "type "):
		return b.cmdContactType(ctx, actor, strings.TrimSpace(args[5:]))
	case strings.HasPrefix(lower, "set-type "):
		return b.cmdContactType(ctx, actor, strings.TrimSpace(args[9:]))
	case lower == "info":
		return "Usage: `contact info <n|keyprefix>`"
	case strings.HasPrefix(lower, "info "):
		return b.cmdContactInfo(actor, strings.TrimSpace(args[5:]))
	case lower == "rename":
		return "Usage: `contact rename <n|keyprefix> <new name>`"
	case strings.HasPrefix(lower, "rename "):
		return b.cmdContactRename(ctx, actor, strings.TrimSpace(args[7:]))
	case lower == "reset-path" || lower == "resetpath":
		return "Usage: `contact reset-path <n|keyprefix>`"
	case strings.HasPrefix(lower, "reset-path "):
		return b.cmdContactResetPath(ctx, actor, strings.TrimSpace(args[11:]))
	case strings.HasPrefix(lower, "resetpath "):
		return b.cmdContactResetPath(ctx, actor, strings.TrimSpace(args[10:]))
	}
	return "Try `contact add`, `find`, `list`, `info`, `rename`, `reset-path`, `refresh` or `remove`."
}

// cmdContactFind searches EVERY contact on the node, repeaters and sensors
// included — those are exactly what you are looking for when clearing out
// clutter, and they never appear in the routable listings.
func (b *Bridge) cmdContactFind(actor, needle string) string {
	contacts, err := b.db.Contacts(-1)
	if err != nil {
		return "Could not read the contacts: " + err.Error()
	}
	needle = strings.ToLower(strings.TrimSpace(needle))

	var rows []listRow
	for _, c := range contacts {
		if needle != "" &&
			!strings.Contains(strings.ToLower(c.Name), needle) &&
			!strings.HasPrefix(c.Prefix, needle) {
			continue
		}
		rows = append(rows, listRow{
			Key: c.Prefix, Label: c.Name, PubKey: c.PubKey,
			Type: c.Type, Hops: c.OutPathLen, LastAdvert: c.LastAdvert,
		})
	}
	if len(rows) == 0 {
		return "Nothing matched. This searches **every** contact on the node — repeaters and " +
			"sensors included, not just the ones that can exchange messages."
	}
	b.putSnapshot(actor, rows, true)

	var s strings.Builder
	fmt.Fprintf(&s, "**Contacts on the node** — %d match(es)\n```\n", len(rows))
	shown := 0
	for i, r := range rows {
		if shown >= maxListRows {
			break
		}
		shown++
		name := meshcore.TruncateUTF8(r.Label, 22)
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(&s, "%2d %s %s %s\n", i+1, PadDisplay(name, 22),
			PadDisplay(meshcore.AdvTypeName(byte(r.Type)), 9), r.Key)
	}
	s.WriteString("```")
	if len(rows) > shown {
		fmt.Fprintf(&s, "\n_%d more — narrow it with `contact find <text>`._", len(rows)-shown)
	}
	s.WriteString("\nRemove one with `contact remove <n>`. Numbering holds for 10 minutes.")
	return s.String()
}

// resolveContact turns a listing number or a key prefix into a cached contact.
//
// Prefixes work here even though the node needs a full key for most
// operations: the mirror holds full keys, so the bridge can do the lookup that
// the protocol itself cannot (CMD_GET_CONTACT_BY_KEY needs the full key it is
// trying to find).
func (b *Bridge) resolveContact(actor, token string) (store.Contact, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return store.Contact{}, fmt.Errorf("Give a number from the last `contact find`, or a key prefix.")
	}

	if lower := trimLower(token); len(lower) == meshcore.PubKeySize*2 {
		if _, err := meshcore.ParsePubKey(lower); err != nil {
			return store.Contact{}, fmt.Errorf("That key is not hexadecimal.")
		}
		if c, ok := b.db.ContactByPrefix(lower[:12]); ok {
			return c, nil
		}
		return store.Contact{PubKey: lower, Prefix: lower[:12]}, nil
	}
	if prefix, ok := isKeyPrefix(token); ok {
		if c, found := b.db.ContactByPrefix(prefix); found {
			return c, nil
		}
		return store.Contact{}, fmt.Errorf("`%s` is not in the radio's contact list. "+
			"Run `contact find` to see what is.", prefix)
	}

	n, err := strconv.Atoi(token)
	if err != nil || n <= 0 {
		return store.Contact{}, fmt.Errorf("Give a number from the last `contact find`, or a key prefix.")
	}
	snap, ok := b.getSnapshot(actor)
	if !ok || !snap.contacts {
		return store.Contact{}, fmt.Errorf("That listing has expired. Run `contact find` again.")
	}
	if n > len(snap.rows) {
		return store.Contact{}, fmt.Errorf("No item %d in the last scan (1-%d).", n, len(snap.rows))
	}
	row := snap.rows[n-1]
	if c, found := b.db.ContactByPrefix(row.Key); found {
		return c, nil
	}
	return store.Contact{PubKey: row.PubKey, Prefix: row.Key, Name: row.Label,
		Type: row.Type, OutPathLen: row.Hops}, nil
}

// cmdContactInfo shows everything known about one contact — above all its FULL
// public key, which nothing else in Discord displays and which is what you need
// to share a node, remove it, or add it on another radio.
func (b *Bridge) cmdContactInfo(actor, token string) string {
	c, err := b.resolveContact(actor, token)
	if err != nil {
		return err.Error()
	}
	name := c.Name
	if name == "" {
		name = "(unnamed)"
	}

	var s strings.Builder
	fmt.Fprintf(&s, "**%s**\n```\n", name)
	fmt.Fprintf(&s, "type       : %s\n", meshcore.AdvTypeName(byte(c.Type)))
	fmt.Fprintf(&s, "key prefix : %s\n", c.Prefix)
	fmt.Fprintf(&s, "path       : %s\n", HopsLabel(c.OutPathLen))
	if !c.LastAdvert.IsZero() {
		fmt.Fprintf(&s, "last heard : %s\n", c.LastAdvert.Format("2006-01-02 15:04"))
	} else {
		s.WriteString("last heard : never\n")
	}
	if c.Lat != 0 || c.Lon != 0 {
		fmt.Fprintf(&s, "position   : %.4f, %.4f\n", c.Lat, c.Lon)
	}
	s.WriteString("```\n")
	// On its own line in a code block: Discord gives that a copy button on
	// desktop and makes it one tap to select on mobile. Nobody should have to
	// transcribe 64 hex characters from a screen.
	s.WriteString("Full public key:\n```\n" + c.PubKey + "\n```")

	if _, err := b.db.RouteByPrefix(c.Prefix); err == nil {
		s.WriteString("\nLinked to a channel already.")
	} else if c.Type == meshcore.AdvTypeChat || c.Type == meshcore.AdvTypeRoom {
		s.WriteString("\nNot linked. `add " + c.Prefix + "` gives it a channel.")
	}
	return s.String()
}

// cmdContactRename changes a contact's name on the radio.
func (b *Bridge) cmdContactRename(ctx context.Context, actor, args string) string {
	sess := b.link.Session()
	if sess == nil {
		return "Not connected to the node right now."
	}
	token, newName, _ := strings.Cut(args, " ")
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return "Usage: `contact rename <n|keyprefix> <new name>`"
	}
	c, err := b.resolveContact(actor, token)
	if err != nil {
		return err.Error()
	}
	old := c.Name
	if old == "" {
		old = c.Prefix
	}
	if err := sess.UpdateContact(ctx, c.Prefix, newName, byte(c.Type)); err != nil {
		return "The node would not rename that contact: " + err.Error()
	}
	b.persistContacts(sess)
	// The link label is cosmetic and separate, so update it too or the channel
	// keeps showing the old name.
	_ = b.db.UpdateRouteLabel(store.KindDM, c.Prefix, newName)
	_ = b.db.UpdateRouteLabel(store.KindRoom, c.Prefix, newName)
	b.db.LogEvent("info", "admin", fmt.Sprintf("renamed contact %s to %q", c.Prefix, newName))
	return fmt.Sprintf("Renamed **%s** to **%s** on the radio. Its stored path was kept.", old, newName)
}

// parseAdvType maps the words a human would type to a MeshCore contact type.
func parseAdvType(s string) (byte, bool) {
	switch trimLower(s) {
	case "room", "roomserver", "room-server":
		return meshcore.AdvTypeRoom, true
	case "companion", "chat", "person":
		return meshcore.AdvTypeChat, true
	case "repeater":
		return meshcore.AdvTypeRepeater, true
	case "sensor":
		return meshcore.AdvTypeSensor, true
	}
	return 0, false
}

// contactTypeHelp is the one-line reminder of what the types are.
const contactTypeHelp = "`room` (a room server), `companion` (a person), `repeater` or `sensor`"

// cmdContactType corrects what a contact IS.
//
// Getting this wrong is easy — `contact add <key> <name>` without the trailing
// `room` makes a room server look like a person — and the consequences are
// quiet rather than loud: linking puts it under Companion DMs, `login` refuses
// it as "not a room server", and posts then go out and vanish, because a room
// server silently drops anything from someone who has not logged in.
//
// Re-running `contact add` would also fix the type, but it resends the record
// with no path, throwing away a learned route. This keeps it.
func (b *Bridge) cmdContactType(ctx context.Context, actor, args string) string {
	// Validate before consulting the radio: a typo in the type is the user's
	// problem to fix either way, and "not connected" would hide it.
	token, want, _ := strings.Cut(args, " ")
	want = trimLower(want)
	if want == "" {
		return "Usage: `contact type <n|keyprefix> room|companion|repeater|sensor`"
	}

	advType, ok := parseAdvType(want)
	if !ok {
		return "Type must be one of " + contactTypeHelp + "."
	}

	c, err := b.resolveContact(actor, token)
	if err != nil {
		return err.Error()
	}
	if c.Name == "" {
		return "That contact has no name on file; run `contact refresh` and try again."
	}
	was := meshcore.AdvTypeName(byte(c.Type))
	if byte(c.Type) == advType {
		return fmt.Sprintf("**%s** is already a %s.", c.Name, was)
	}

	sess := b.link.Session()
	if sess == nil {
		return "Not connected to the node right now."
	}
	if err := sess.UpdateContact(ctx, c.Prefix, c.Name, advType); err != nil {
		return "The node would not change that contact: " + err.Error()
	}
	b.persistContacts(sess)
	b.clearSnapshots() // the listing that produced the row number is now stale
	b.db.LogEvent("info", "admin", fmt.Sprintf("contact %s retyped %s -> %s",
		c.Prefix, was, meshcore.AdvTypeName(advType)))

	msg := fmt.Sprintf("**%s** is now a %s (was a %s). Its stored path was kept.",
		c.Name, meshcore.AdvTypeName(advType), was)

	// If it is already linked, the link kind is now wrong and routing follows
	// the link, not the contact — so say so rather than leaving it half-fixed.
	if r, rerr := b.db.RouteByPrefix(c.Prefix); rerr == nil {
		wantKind := store.KindDM
		if advType == meshcore.AdvTypeRoom {
			wantKind = store.KindRoom
		}
		if r.Kind != wantKind {
			msg += fmt.Sprintf("\n\n**It is still linked as a %s**, and routing follows the link. "+
				"Run `remove %s` then `add %s` to relink it correctly. The old Discord channel is "+
				"left in place for you to delete.", r.Kind, c.Prefix, c.Prefix)
		}
	} else if advType == meshcore.AdvTypeRoom {
		msg += "\nLink it with `add " + c.Prefix + "`, then set its password with `/mesh login`."
	}
	return msg
}

// cmdContactResetPath forgets a contact's stored route.
//
// Worth having as a command rather than only as a `path:flood` message prefix:
// a stale path keeps being used until something proves it wrong, and clearing
// it is the fix when someone has moved.
func (b *Bridge) cmdContactResetPath(ctx context.Context, actor, token string) string {
	sess := b.link.Session()
	if sess == nil {
		return "Not connected to the node right now."
	}
	c, err := b.resolveContact(actor, token)
	if err != nil {
		return err.Error()
	}
	if err := sess.ResetPath(ctx, c.Prefix); err != nil {
		return "The node would not clear that path: " + err.Error()
	}
	b.persistContacts(sess)
	name := c.Name
	if name == "" {
		name = c.Prefix
	}
	return fmt.Sprintf("Cleared the stored path to **%s**. The next message floods, and the path "+
		"is relearned from the reply.", name)
}

// cmdContactRefresh re-reads the contact list from the radio.
func (b *Bridge) cmdContactRefresh(ctx context.Context) string {
	sess := b.link.Session()
	if sess == nil {
		return "Not connected to the node right now."
	}
	n, complete, err := sess.RefreshContacts(ctx)
	if err != nil {
		return "Could not read the contacts: " + err.Error()
	}
	b.syncContacts(sess, complete)
	b.clearSnapshots() // any frozen numbering now refers to a stale list
	if !complete {
		return fmt.Sprintf("Read %d contact(s), but the radio stopped answering before the end "+
			"of its list — so this is a partial read and nothing was removed. Try again in a "+
			"moment if something looks missing.", n)
	}
	return fmt.Sprintf("Re-read %d contact(s) from the radio.", n)
}

func (b *Bridge) cmdContactAdd(ctx context.Context, args string) string {
	// Type first, name last. It used to be inferred from a trailing "room",
	// which was wrong twice over: leaving it off silently produced a companion
	// — so a room server would refuse logins and swallow posts with no
	// explanation — and a contact genuinely called "KB0STG Wio Room" had the
	// word eaten out of its name. A required keyword in a fixed position
	// cannot do either.
	key, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	typeTok, rest2, _ := strings.Cut(strings.TrimSpace(rest), " ")
	rest2 = strings.TrimSpace(rest2)

	// The optional +link flag sits between the type and the name, and carries a
	// leading "+" so it cannot be mistaken for the first word of a name. Names
	// may contain spaces; flags may not.
	alsoLink := false
	if flag, after, found := strings.Cut(rest2, " "); found && trimLower(flag) == "+link" {
		alsoLink, rest2 = true, strings.TrimSpace(after)
	} else if trimLower(rest2) == "+link" {
		alsoLink, rest2 = true, ""
	}
	name := rest2

	key = trimLower(key)
	if key == "" {
		return b.contactAddUsage("")
	}
	if len(key) != meshcore.PubKeySize*2 {
		return fmt.Sprintf(
			"That key is %d characters. A full public key is 64 hex characters. The 12-character "+
				"prefix shown next to a message is not enough to add a contact, though "+
				"`add <prefix>` can still link a channel for them.", len(key))
	}
	if _, err := meshcore.ParsePubKey(key); err != nil {
		return "That key is not hexadecimal."
	}

	advType, ok := parseAdvType(typeTok)
	if !ok {
		if typeTok == "" {
			return b.contactAddUsage("You did not say what it is.")
		}
		return b.contactAddUsage(fmt.Sprintf("`%s` is not a contact type.", typeTok))
	}

	// A name is required for the same reason: without one the contact shows as
	// "(unnamed)" everywhere, any channel created for it is named after hex,
	// and it cannot be found by name later.
	if name == "" {
		return b.contactAddUsage("You did not give it a name.")
	}

	sess := b.link.Session()
	if sess == nil {
		return "Not connected to the node right now."
	}
	if err := sess.AddContact(ctx, key, name, advType); err != nil {
		return "The node rejected that contact: " + err.Error()
	}
	// Mirror the cache to the database, but do NOT re-enumerate: AddContact
	// has already put the new contact in the cache, and a full enumeration
	// streams one frame per contact — on a 325-contact mesh over BLE that
	// turned a one-command operation into a wait of tens of seconds, for
	// information already known.
	b.persistContacts(sess)

	prefix := key[:12]
	msg := fmt.Sprintf("Added **%s** as a %s.", name, meshcore.AdvTypeName(advType))

	if alsoLink {
		switch advType {
		case meshcore.AdvTypeChat, meshcore.AdvTypeRoom:
			kind := store.KindDM
			if advType == meshcore.AdvTypeRoom {
				kind = store.KindRoom
			}
			msg += "\n" + b.CreateLink(ctx, kind, prefix, "")
		default:
			// A repeater or sensor cannot exchange messages, so a channel for
			// one would sit empty forever.
			msg += fmt.Sprintf("\nNot linked: a %s cannot exchange messages, so a channel for it "+
				"would never carry anything.", meshcore.AdvTypeName(advType))
		}
	} else {
		msg += fmt.Sprintf("\nKey prefix `%s`. Link a channel for it with `add %s`, or add "+
			"`+link` next time to do both at once.", prefix, prefix)
	}

	if advType == meshcore.AdvTypeRoom {
		msg += "\nSet its password with `/mesh login` — room servers silently drop posts from " +
			"anyone not logged in."
	}
	return msg
}

// contactAddUsage explains the command, leading with what went wrong.
func (b *Bridge) contactAddUsage(problem string) string {
	out := ""
	if problem != "" {
		out = "**" + problem + "**\n"
	}
	return out + "Usage: `contact add <64-hex-key> <type> [+link] <name>`\n" +
		"Type is " + contactTypeHelp + ", and it is required — getting it wrong is quiet rather " +
		"than loud, so it is not guessed. `+link` also creates its Discord channel.\n" +
		"```\ncontact add a1b2c3d4e5f60718… room +link Ridge Room\n```\n" +
		"_The key is the node's full public key, as shown on the public map. Already added one " +
		"with the wrong type? `contact type <keyprefix> room` fixes it._"
}

// cmdContactRemove deletes a contact from the node.
//
// This needs the FULL 32-byte key, which no listing shows and nobody is going
// to retype — which is why a `contact find` snapshot remembers full keys, so a
// row number is enough to act on.
func (b *Bridge) cmdContactRemove(ctx context.Context, actor, args string) string {
	// The optional +delete flag also removes the Discord channel. Off by
	// default and explicit when used, because everywhere else in this bridge a
	// channel is the user's to delete — it may hold conversation history that
	// removing a contact from the radio has no business destroying.
	token, alsoDelete := args, false
	if rest, found := strings.CutSuffix(strings.TrimSpace(args), " +delete"); found {
		token, alsoDelete = strings.TrimSpace(rest), true
	}

	// A prefix is enough here even though the node needs the full key, because
	// the mirror can resolve one. The ESP32 had to refuse and send you back to
	// `contact find` for a row number.
	c, rerr := b.resolveContact(actor, token)
	if rerr != nil {
		return rerr.Error()
	}
	fullKey := c.PubKey
	if fullKey == "" {
		return "No full key on file for that contact. Run `contact refresh`, then try again."
	}
	label := c.Name
	if label == "" {
		label = c.Prefix
	}

	sess := b.link.Session()
	if sess == nil {
		return "Not connected to the node right now."
	}
	if err := sess.RemoveContact(ctx, fullKey); err != nil {
		return "The node would not remove that contact. It may already be gone — run " +
			"`contact find` to check."
	}
	b.persistContacts(sess)

	// Tidy up anything the bridge was holding for it.
	prefix := fullKey[:12]
	extra := ""
	route, hadRoute := b.db.RouteByPrefix(prefix)
	if hadRoute == nil {
		_, _ = b.db.DeleteRoute(route.Kind, prefix)
		switch {
		case !alsoDelete:
			extra += " Its link was removed too; the Discord channel is left in place " +
				"(`+delete` removes that as well)."
		case b.rest.DeleteChannel(ctx, route.ChannelID) == nil:
			extra += " Its link and its Discord channel were removed too."
		default:
			extra += " Its link was removed, but I could not delete the Discord channel — " +
				"check the bot has Manage Channels."
		}
	}
	if b.db.HasRoomPassword(prefix) {
		_ = b.db.SetRoomPassword(prefix, "")
		extra += " Its stored room password has been forgotten."
	}
	b.clearSnapshots() // the numbering refers to a list that has changed

	return fmt.Sprintf("Removed **%s** `%s` from the node.%s\n_The contact comes back if that "+
		"node adverts again._", label, prefix, extra)
}

// ---------------------------------------------------------------------------
// Room-server login
// ---------------------------------------------------------------------------

// cmdLogin stores a room server's password and uses it.
//
// The password arrives as a plain Discord message, so the very first thing
// done with it is deleting that message. That needs MANAGE_MESSAGES; without
// it the password is still sitting in the channel and the user has to be told
// plainly rather than left assuming it was handled.
//
// `/mesh login` avoids all of this — the modal means the password never
// reaches channel history in the first place.
func (b *Bridge) cmdLogin(ctx context.Context, actor, args, channelID, messageID string) string {
	// Delete FIRST. Everything below can reply, take airtime, or fail, and
	// none of that should extend how long the password sits in the channel.
	wiped := false
	if messageID != "" && channelID != "" {
		wiped = b.rest.DeleteMessage(ctx, channelID, messageID) == nil
	}

	if args == "" {
		return "Usage: `login <n|keyprefix> <password>`\n" +
			"Better: run `/mesh login` and type it into the popup, so it never enters channel " +
			"history at all.\n`login <n|keyprefix>` with no password forgets the stored one."
	}

	// No password means "forget it", which is why a missing space is fine.
	token, password, _ := strings.Cut(args, " ")
	password = strings.TrimSpace(password)

	row, err := b.resolveTarget(actor, token)
	if err != nil {
		return err.Error()
	}
	if row.Kind != store.KindRoom {
		return "**" + row.Label + "** is not a room server. Only room servers take a login — mesh " +
			"channels use a shared secret held on your node, and direct messages need no login."
	}
	if password == "" {
		_ = b.db.SetRoomPassword(row.Key, "")
		return "Forgot the stored password for **" + row.Label + "**."
	}

	note := b.SetRoomPassword(ctx, row.Key, row.Label, password)
	if !wiped && messageID != "" {
		note += "\n\n**I could not delete your message, so the password is still visible above.** " +
			"Delete it yourself, and give the bot the **Manage Messages** permission so I can do " +
			"it next time — or use `/mesh login`, where this cannot happen."
	}
	return note
}

// SetRoomPassword stores a password and attempts a login, returning what to
// tell the user. Shared by the typed command and the modal.
func (b *Bridge) SetRoomPassword(ctx context.Context, prefix, label, password string) string {
	if err := b.db.SetRoomPassword(prefix, password); err != nil {
		return "Could not save the password: " + err.Error()
	}
	if label == "" {
		label = prefix
	}
	note := "Password stored for **" + label + "** `" + prefix + "`. "

	sess := b.link.Session()
	switch {
	case sess == nil:
		return note + "The node is not connected right now, so I will log in as soon as it is back."
	case b.tryRoomLoginNow(ctx, prefix, password):
		return note + "Logging in now — the room replies over the air, so give it a few seconds. " +
			"I will report the result in its channel."
	default:
		return note + "**The node would not send the login.** That usually means the room is not " +
			"in its contact list; `add " + prefix + "` first."
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
