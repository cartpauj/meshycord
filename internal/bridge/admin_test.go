package bridge

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meshycord/internal/config"
	"meshycord/internal/discord"
	"meshycord/internal/meshcore"
	"meshycord/internal/store"
)

func newTestBridge(t *testing.T) (*Bridge, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg, err := config.New(db)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, db, log), db
}

func seedBridge(t *testing.T, db *store.Store) {
	t.Helper()
	contacts := []store.Contact{
		{PubKey: strings.Repeat("a1", 32), Prefix: "a1a1a1a1a1a1", Type: meshcore.AdvTypeChat,
			Name: "Alice", OutPathLen: 2, LastAdvert: time.Now()},
		{PubKey: strings.Repeat("b2", 32), Prefix: "b2b2b2b2b2b2", Type: meshcore.AdvTypeRoom,
			Name: "Ridge Room", OutPathLen: 255, LastAdvert: time.Now().Add(-time.Hour)},
		{PubKey: strings.Repeat("c3", 32), Prefix: "c3c3c3c3c3c3", Type: meshcore.AdvTypeRepeater,
			Name: "Hilltop Repeater", OutPathLen: 1, LastAdvert: time.Now().Add(-2 * time.Hour)},
		{PubKey: strings.Repeat("d4", 32), Prefix: "d4d4d4d4d4d4", Type: meshcore.AdvTypeChat,
			Name: "Bob", OutPathLen: 4, LastAdvert: time.Now().Add(-30 * time.Minute)},
	}
	if err := db.ReplaceContacts(contacts); err != nil {
		t.Fatalf("contacts: %v", err)
	}
	if _, err := db.PutRoute(store.KindDM, "a1a1a1a1a1a1", "chan-alice", "Alice"); err != nil {
		t.Fatalf("route: %v", err)
	}
}

func exec(t *testing.T, b *Bridge, line string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return b.Exec(ctx, "tester", line, "", "")
}

func TestHelpAndStatusRender(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	help := exec(t, b, "help")
	for _, want := range []string{"list rooms", "add <n>", "contact add", "login", "path:flood"} {
		if !strings.Contains(help, want) {
			t.Errorf("help is missing %q", want)
		}
	}
	// Discord caps a message at 2000 characters; a help text that exceeds it
	// gets truncated in the middle of a sentence.
	if len(help) > 2000 {
		t.Errorf("help is %d characters, over Discord's 2000 limit", len(help))
	}

	status := exec(t, b, "status")
	for _, want := range []string{"mesh link", "DOWN", "discord", "links", "uptime"} {
		if !strings.Contains(status, want) {
			t.Errorf("status is missing %q", want)
		}
	}
}

func TestListingsAndFiltering(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	rooms := exec(t, b, "list rooms")
	if !strings.Contains(rooms, "Ridge Room") {
		t.Error("`list rooms` did not show the room server")
	}
	if strings.Contains(rooms, "Alice") {
		t.Error("`list rooms` showed a companion")
	}

	people := exec(t, b, "list companions")
	if !strings.Contains(people, "Alice") || !strings.Contains(people, "Bob") {
		t.Error("`list companions` is missing someone")
	}
	if !strings.Contains(people, "[x]") {
		t.Error("an already-linked contact is not marked")
	}

	// Repeaters never appear in the routable listings, but `list repeaters`
	// exists because they are exactly what you look for when clearing clutter.
	if got := exec(t, b, "list repeaters"); !strings.Contains(got, "Hilltop") {
		t.Error("`list repeaters` did not show the repeater")
	}
	if got := exec(t, b, "list companions"); strings.Contains(got, "Hilltop") {
		t.Error("a repeater leaked into `list companions`")
	}

	if got := exec(t, b, "list links"); !strings.Contains(got, "Alice") {
		t.Error("`list links` is missing the one link")
	}

	unlinked := exec(t, b, "list companions unlinked")
	if strings.Contains(unlinked, "Alice") {
		t.Error("`unlinked` still showed a linked contact")
	}
	if !strings.Contains(unlinked, "Bob") {
		t.Error("`unlinked` dropped an unlinked contact")
	}

	if got := exec(t, b, "find ridge"); !strings.Contains(got, "Ridge Room") {
		t.Error("find by name failed")
	}
	// A key prefix has to work too: it is what a message shows next to a name.
	if got := exec(t, b, "find b2b2"); !strings.Contains(got, "Ridge Room") {
		t.Error("find by key prefix failed")
	}
	if got := exec(t, b, "find nothingmatchesthis"); !strings.Contains(got, "Nothing matched") {
		t.Errorf("a hopeless search returned %q", got)
	}
}

// `find ridge name` searches for "ridge" and sorts by name — the trailing
// modifier must not become part of the search text.
func TestSearchModifiersAreStrippedFromTheQuery(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	if got := exec(t, b, "find ridge name"); !strings.Contains(got, "Ridge Room") {
		t.Errorf("modifier was searched for as text: %q", got)
	}
	if got := exec(t, b, "find alice unlinked"); !strings.Contains(got, "Nothing matched") {
		t.Error("`unlinked` was ignored: Alice is linked and should be filtered out")
	}
}

// Listing numbering freezes for ten minutes, so `add 7` always means the row
// you saw — and it is per-person, so two people listing at once do not
// renumber each other's rows.
func TestSnapshotNumberingIsFrozenAndPerActor(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)
	ctx := context.Background()

	b.Exec(ctx, "alice", "list companions", "", "")
	// Bob has not listed anything, so a number means nothing to him.
	if got := b.Exec(ctx, "bob", "remove 1", "", ""); !strings.Contains(got, "expired") {
		t.Errorf("bob's numbering resolved against alice's listing: %q", got)
	}

	// Alice's listing still resolves.
	if got := b.Exec(ctx, "alice", "remove 1", "", ""); strings.Contains(got, "expired") {
		t.Errorf("alice's own listing expired immediately: %q", got)
	}

	// An out-of-range row says so rather than acting on the wrong thing.
	if got := b.Exec(ctx, "alice", "remove 99", "", ""); !strings.Contains(got, "No item 99") {
		t.Errorf("out-of-range row = %q", got)
	}
}

func TestUnlinkForgetsTheRoomPassword(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	if _, err := db.PutRoute(store.KindRoom, "b2b2b2b2b2b2", "chan-room", "Ridge Room"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRoomPassword("b2b2b2b2b2b2", "fake-test-password"); err != nil {
		t.Fatal(err)
	}

	got := exec(t, b, "remove b2b2b2b2b2b2")
	if !strings.Contains(got, "Unlinked") {
		t.Fatalf("unlink said %q", got)
	}
	if !strings.Contains(got, "password has been forgotten") {
		t.Error("the user was not told the password was dropped")
	}
	if db.HasRoomPassword("b2b2b2b2b2b2") {
		t.Error("the stored room password survived the unlink")
	}
	// The Discord channel is never deleted on an unlink — that is the user's
	// call, and the message says so.
	if !strings.Contains(got, "left in place") {
		t.Error("the reply does not say the Discord channel was left alone")
	}
}

func TestLoginRejectsThingsThatAreNotRoomServers(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	got := exec(t, b, "login a1a1a1a1a1a1 secret")
	if !strings.Contains(got, "not a room server") {
		t.Errorf("logging in to a person was allowed: %q", got)
	}
	// The password must not have been stored for a non-room target.
	if db.HasRoomPassword("a1a1a1a1a1a1") {
		t.Error("a password was stored for a contact that is not a room server")
	}

	if got := exec(t, b, "login"); !strings.Contains(got, "Usage") {
		t.Errorf("bare `login` = %q", got)
	}
	// The modal is the better route and the help should say so.
	if !strings.Contains(exec(t, b, "login"), "/mesh login") {
		t.Error("`login` does not point at the private popup")
	}
}

func TestLoginStoresAndForgetsAPassword(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	got := exec(t, b, "login b2b2b2b2b2b2 fake-test-password")
	if !strings.Contains(got, "Password stored") {
		t.Fatalf("login said %q", got)
	}
	if db.RoomPassword("b2b2b2b2b2b2") != "fake-test-password" {
		t.Error("the password was not stored")
	}
	// With no radio attached it must say it will log in later rather than
	// claiming success.
	if !strings.Contains(got, "not connected") {
		t.Errorf("with no radio, login claimed more than it should: %q", got)
	}

	if got := exec(t, b, "login b2b2b2b2b2b2"); !strings.Contains(got, "Forgot") {
		t.Errorf("forgetting a password said %q", got)
	}
	if db.HasRoomPassword("b2b2b2b2b2b2") {
		t.Error("the password survived being forgotten")
	}
}

func TestContactCommandsValidateKeys(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	// No radio attached: every command that changes the node must say so
	// rather than failing obscurely.
	if got := exec(t, b, "contact add "+strings.Repeat("aa", 32)+" companion Somebody"); !strings.Contains(got, "Not connected") {
		t.Errorf("contact add with no radio = %q", got)
	}
	// Resolution happens before the link is consulted, so a target that cannot
	// be resolved says so rather than blaming the radio.
	if got := exec(t, b, "contact remove 1"); !strings.Contains(got, "expired") {
		t.Errorf("contact remove with no listing = %q, want the expiry hint", got)
	}
	if got := exec(t, b, "contact remove a1a1a1a1a1a1"); !strings.Contains(got, "Not connected") {
		t.Errorf("contact remove of a real contact with no radio = %q", got)
	}

	// The listing works without a radio, because it reads the mirror.
	got := exec(t, b, "contact find")
	if !strings.Contains(got, "Hilltop") || !strings.Contains(got, "repeater") {
		t.Errorf("`contact find` did not list every contact type: %q", got)
	}
	if !strings.Contains(got, "Alice") {
		t.Error("`contact find` dropped a companion")
	}
}

func TestUnknownCommand(t *testing.T) {
	b, _ := newTestBridge(t)
	if got := exec(t, b, "frobnicate the widget"); !strings.Contains(got, "Unknown command") {
		t.Errorf("got %q", got)
	}
	if got := exec(t, b, "   "); got != "" {
		t.Errorf("whitespace produced a reply: %q", got)
	}
}

func TestResetAsksBeforeActing(t *testing.T) {
	b, _ := newTestBridge(t)
	got := exec(t, b, "reset")
	if !strings.Contains(got, "reset confirm") {
		t.Errorf("`reset` did not ask for confirmation: %q", got)
	}
	if !strings.Contains(got, "untouched") {
		t.Error("`reset` does not reassure that hand-made channels are safe")
	}
}

func TestSyncRoomsAsksBeforeActing(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	got := exec(t, b, "sync rooms")
	if !strings.Contains(got, "sync rooms confirm") {
		t.Errorf("`sync rooms` did not ask first: %q", got)
	}
	if !strings.Contains(got, "1") {
		t.Errorf("`sync rooms` did not report the count: %q", got)
	}

	// With the only room already linked there is nothing to do.
	if _, err := db.PutRoute(store.KindRoom, "b2b2b2b2b2b2", "chan-room", "Ridge Room"); err != nil {
		t.Fatal(err)
	}
	if got := exec(t, b, "sync rooms"); !strings.Contains(got, "already linked") {
		t.Errorf("got %q", got)
	}
}

// Every reply has to fit in a Discord message.
func TestRepliesStayInsideDiscordsMessageLimit(t *testing.T) {
	b, db := newTestBridge(t)

	// A mesh with hundreds of contacts is normal; a listing of all of them
	// must not produce a message Discord refuses.
	var many []store.Contact
	for i := 0; i < 400; i++ {
		key := strings.Repeat("f", 62) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		many = append(many, store.Contact{
			PubKey: key, Prefix: key[:12], Type: meshcore.AdvTypeChat,
			Name: "Contact with a fairly long display name " + key[:6], OutPathLen: 3,
		})
	}
	if err := db.ReplaceContacts(many); err != nil {
		t.Fatal(err)
	}

	for _, cmd := range []string{"list companions", "contact find", "find contact"} {
		got := exec(t, b, cmd)
		if len(got) > 2000 {
			t.Errorf("%q produced %d characters, over Discord's 2000 limit", cmd, len(got))
		}
		if !strings.Contains(got, "more") {
			t.Errorf("%q did not say how many rows were withheld", cmd)
		}
	}
}

// `!promote <keyprefix>` is a shortcut for `add <keyprefix>` from inside a
// linked channel, carried over from the ESP32 firmware.
func TestPromoteValidatesItsArgument(t *testing.T) {
	for _, bad := range []string{"", "   ", "nothex", "aabb", strings.Repeat("a", 64)} {
		if _, ok := isKeyPrefix(bad); ok {
			t.Errorf("%q was accepted as a key prefix", bad)
		}
	}
	// 12 and 8 characters are both real prefix lengths: 8 is the author prefix
	// a room post carries.
	for _, good := range []string{"a1a1a1a1a1a1", "cafebabe", "A1A1A1A1A1A1"} {
		if _, ok := isKeyPrefix(good); !ok {
			t.Errorf("%q was rejected as a key prefix", good)
		}
	}
}

// A Gateway RESUME replays events after the last acknowledged sequence number.
// Acting on a redelivered message would put the same text on the air twice.
func TestDuplicateDiscordMessagesAreIgnored(t *testing.T) {
	b, _ := newTestBridge(t)

	if !b.firstSighting("111") {
		t.Fatal("a new message id was reported as a duplicate")
	}
	if b.firstSighting("111") {
		t.Error("a repeated message id was not caught")
	}
	if !b.firstSighting("222") {
		t.Error("a different message id was wrongly treated as a duplicate")
	}
	// An empty id cannot be tracked, and must not block anything.
	if !b.firstSighting("") || !b.firstSighting("") {
		t.Error("an empty id should never be treated as a duplicate")
	}

	// The set is bounded, so a long-running bridge cannot grow it without
	// limit.
	for i := 0; i < seenMessagesMax*3; i++ {
		b.firstSighting(itoa(i))
	}
	b.seenMu.Lock()
	n := len(b.seen)
	b.seenMu.Unlock()
	if n > seenMessagesMax {
		t.Errorf("the duplicate guard grew to %d entries, over the %d cap", n, seenMessagesMax)
	}
}

// The channel sync needs both halves up, and they arrive in either order.
func TestChannelSyncWaitsForBothHalves(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)
	ctx := context.Background()

	// No radio and no Discord: must not act, and must not panic.
	b.syncAfterMesh(ctx)

	// Discord ready but no radio: still nothing to do, because channel names
	// come from the radio.
	b.ready.Store(true)
	b.syncAfterMesh(ctx)

	if routes, _ := db.Routes(); len(routes) != 1 {
		t.Errorf("sync created links with no radio attached: %d routes", len(routes))
	}
}

// Contact management has to be complete in Discord, because that is now the
// only place it is exposed.
func TestContactControlIsCompleteFromDiscord(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	// `contact info` is the only way to see a full 64-character public key —
	// nothing else in Discord shows one, and it is what you need to share a
	// node or add it on another radio.
	info := exec(t, b, "contact info a1a1a1a1a1a1")
	if !strings.Contains(info, strings.Repeat("a1", 32)) {
		t.Errorf("contact info does not show the full public key:\n%s", info)
	}
	for _, want := range []string{"Alice", "companion", "a1a1a1a1a1a1", "path"} {
		if !strings.Contains(info, want) {
			t.Errorf("contact info is missing %q", want)
		}
	}

	// Resolvable by row number from the last listing, too.
	exec(t, b, "contact find")
	if got := exec(t, b, "contact info 1"); !strings.Contains(got, "Full public key") {
		t.Errorf("contact info by row number failed: %q", got)
	}

	// Unknown targets say so rather than acting on the wrong contact.
	if got := exec(t, b, "contact info ffffffffffff"); !strings.Contains(got, "not in the radio") {
		t.Errorf("unknown prefix = %q", got)
	}

	// Everything that changes the node reports the radio being absent, rather
	// than failing obscurely.
	for _, cmd := range []string{
		"contact rename a1a1a1a1a1a1 Alicia",
		"contact reset-path a1a1a1a1a1a1",
		"contact refresh",
		"contact remove a1a1a1a1a1a1",
	} {
		if got := exec(t, b, cmd); !strings.Contains(got, "Not connected") {
			t.Errorf("%q with no radio = %q", cmd, got)
		}
	}

	// Usage help for each, so nobody has to guess the argument order.
	for _, cmd := range []string{"contact info", "contact rename", "contact reset-path"} {
		if got := exec(t, b, cmd); !strings.Contains(got, "Usage") {
			t.Errorf("%q = %q, want usage", cmd, got)
		}
	}
}

// Removing used to demand a row number or all 64 hex characters. The mirror
// holds full keys, so a prefix is enough — the bridge does the lookup the
// protocol cannot do for itself.
func TestContactRemoveAcceptsAPrefix(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	// No radio attached, so this gets as far as resolving and then stops —
	// which is exactly the step being tested.
	if got := exec(t, b, "contact remove a1a1a1a1a1a1"); strings.Contains(got, "not enough") {
		t.Errorf("a prefix was refused: %q", got)
	}
	if got := exec(t, b, "contact remove zzzz"); !strings.Contains(got, "number") {
		t.Errorf("junk target = %q", got)
	}
}

// Every contact subcommand must appear in help, or it may as well not exist.
func TestHelpListsEveryContactSubcommand(t *testing.T) {
	b, _ := newTestBridge(t)
	help := exec(t, b, "help")
	for _, want := range []string{
		"contact add", "contact find", "contact list", "contact info",
		"contact rename", "contact reset-path", "contact refresh", "contact remove",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help does not mention %q", want)
		}
	}
	if len(help) > 2000 {
		t.Errorf("help is %d characters, over Discord's 2000 limit", len(help))
	}
}

// A contact's type decides everything downstream: which category it is linked
// under, whether `login` is offered, and whether posts to it silently vanish.
// Guessing it was a real trap, so it is required.
func TestContactAddDemandsAnExplicitType(t *testing.T) {
	b, _ := newTestBridge(t)
	key := strings.Repeat("f4", 32)

	// The old syntax — key then name, no type — must now be refused, not
	// quietly treated as a companion.
	got := exec(t, b, "contact add "+key+" Tina_You_Fat_Lard")
	if strings.Contains(got, "Not connected") || strings.Contains(got, "Added") {
		t.Fatalf("a typeless add was accepted: %q", got)
	}
	if !strings.Contains(got, "not a contact type") {
		t.Errorf("refusal does not explain the problem: %q", got)
	}
	for _, want := range []string{"room", "companion", "repeater", "sensor", "contact type"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal does not mention %q — the user cannot act on it", want)
		}
	}

	// Missing name, bad type, bad key: each says which one is wrong.
	if got := exec(t, b, "contact add "+key+" room"); !strings.Contains(got, "name") {
		t.Errorf("missing name = %q", got)
	}
	if got := exec(t, b, "contact add "+key+" banana Some Name"); !strings.Contains(got, "not a contact type") {
		t.Errorf("bad type = %q", got)
	}
	if got := exec(t, b, "contact add abc123 room Some Name"); !strings.Contains(got, "64 hex") {
		t.Errorf("short key = %q", got)
	}
	if got := exec(t, b, "contact add "+strings.Repeat("zz", 32)+" room Some Name"); !strings.Contains(got, "hexadecimal") {
		t.Errorf("non-hex key = %q", got)
	}

	// Type before name means a name may contain spaces — and, critically, may
	// itself end in the word "Room" without being eaten. That was a real bug
	// in the old trailing-keyword syntax.
	if got := exec(t, b, "contact add "+key+" room KB0STG Wio Room"); !strings.Contains(got, "Not connected") {
		t.Errorf("a multi-word name ending in Room was not parsed: %q", got)
	}
}

// Fixing a mis-typed contact must be possible without re-adding it, because
// re-adding throws away any learned path.
func TestContactTypeCorrectsAMistake(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	if got := exec(t, b, "contact type"); !strings.Contains(got, "Usage") {
		t.Errorf("bare `contact type` = %q", got)
	}
	if got := exec(t, b, "contact type a1a1a1a1a1a1 banana"); !strings.Contains(got, "must be one of") {
		t.Errorf("bad type = %q", got)
	}
	// With no radio it stops at the link, having resolved everything else.
	if got := exec(t, b, "contact type a1a1a1a1a1a1 room"); !strings.Contains(got, "Not connected") {
		t.Errorf("contact type with no radio = %q", got)
	}
}

// The reaction path is the only resend trigger for now; help must not tell
// people to reply `retry` when that no longer does anything.
func TestHelpDoesNotAdvertiseTheDisabledReplyRetry(t *testing.T) {
	b, _ := newTestBridge(t)
	help := exec(t, b, "help")
	if strings.Contains(help, "reply `retry`") || strings.Contains(help, "`retry`") {
		t.Errorf("help still tells people to reply retry:\n%s", help)
	}
	if !strings.Contains(help, EmojiRetry) {
		t.Error("help does not mention the reaction that does work")
	}
}

// An advert is how this node gets into anybody else's contact list, and the two
// reaches cost very different amounts of other people's airtime. Both words have
// to be understood, and neither may be mistaken for the other.
func TestAdvertCommandDistinguishesReach(t *testing.T) {
	b, _ := newTestBridge(t)

	// No link, so what is checked here is that the command is recognised at all
	// and refuses honestly rather than falling through to "unknown command".
	for _, line := range []string{"advert", "ADVERT", "advert flood", "advert zero-hop"} {
		got := exec(t, b, line)
		if strings.Contains(got, "Unknown command") {
			t.Errorf("%q was not recognised as a command", line)
		}
		if !strings.Contains(got, "Not connected") {
			t.Errorf("%q with no link said: %q", line, got)
		}
	}

	// And it is in the help, or nobody will find it.
	if help := exec(t, b, "help"); !strings.Contains(help, "advert") {
		t.Error("help does not mention advert")
	}
}

// The slash command must carry both reaches, and must default to the cheap one:
// a flood is passed on by every repeater, so it is not something to do by
// accident.
func TestAdvertSlashCommandDefaultsToZeroHop(t *testing.T) {
	var advert *discord.AppCommandOption
	for i, o := range meshCommands[0].Options {
		if o.Name == "advert" {
			advert = &meshCommands[0].Options[i]
		}
	}
	if advert == nil {
		t.Fatal("/mesh has no advert subcommand")
	}
	if len(advert.Options) != 1 || advert.Options[0].Name != "reach" {
		t.Fatalf("advert takes %d options, want one named reach", len(advert.Options))
	}
	if advert.Options[0].Required {
		t.Error("reach is required, so the cheap default cannot apply")
	}
	var values []string
	for _, c := range advert.Options[0].Choices {
		v, _ := c.Value.(string)
		values = append(values, v)
	}
	if len(values) != 2 || values[0] != "zero-hop" || values[1] != "flood" {
		t.Errorf("reach choices = %v, want zero-hop then flood", values)
	}
}
