package bridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"meshycord/internal/discord"
	"meshycord/internal/store"
)

// Slash commands, buttons and modals.
//
// All of this was flatly impossible on the ESP32: an interaction needs either
// a Gateway connection or a public HTTPS endpoint, and a device that polls
// REST has neither. The one that matters most is the modal — a private popup
// whose contents never enter channel history, which is the correct way to
// collect a room-server password. The old approach was to type it as a channel
// message and delete it afterwards, which was a compromise rather than a
// design.
//
// Every command below funnels into the same Exec() the typed console uses, so
// the two interfaces cannot drift apart.

// Custom IDs for components. Kept short: Discord caps them at 100 characters.
const (
	idLoginButton = "login:"      // login:<prefix>
	idLoginModal  = "loginmodal:" // loginmodal:<prefix>
	idLoginField  = "password"
	idResetYes    = "reset:yes"
	idResetNo     = "reset:no"
)

// meshCommands is the /mesh command tree.
var meshCommands = []discord.AppCommand{{
	Name:        "mesh",
	Description: "Manage the MeshCore bridge",
	Options: []discord.AppCommandOption{
		{Type: 1, Name: "status", Description: "Show the bridge, radio and Discord status"},
		{Type: 1, Name: "help", Description: "List every command"},
		{
			Type: 1, Name: "list", Description: "List mesh sources and links",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "what", Description: "What to list", Required: true,
				Choices: []discord.AppCommandChoice{
					{Name: "room servers", Value: "rooms"},
					{Name: "companions", Value: "companions"},
					{Name: "mesh channels", Value: "channels"},
					{Name: "links", Value: "links"},
					{Name: "repeaters", Value: "repeaters"},
					{Name: "sensors", Value: "sensors"},
				},
			}, {
				Type: discord.OptionBoolean, Name: "unlinked",
				Description: "Only show things that have no Discord channel yet",
			}, {
				Type: discord.OptionString, Name: "sort", Description: "Sort order",
				Choices: []discord.AppCommandChoice{
					{Name: "most recently heard", Value: "recent"},
					{Name: "name", Value: "name"},
					{Name: "hops", Value: "hops"},
				},
			}},
		},
		{
			Type: 1, Name: "find", Description: "Search contacts and channels by name or key",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "text", Description: "Name or key prefix", Required: true,
			}},
		},
		{
			Type: 1, Name: "link", Description: "Give a mesh source its own Discord channel",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "target", Required: true,
				Description: "A number from the last list, or a 12-character key prefix",
			}, {
				Type: discord.OptionString, Name: "name",
				Description: "Channel name to use (cosmetic; routing is always by key)",
			}},
		},
		{
			Type: 1, Name: "unlink", Description: "Remove a link (the Discord channel is left alone)",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "target", Required: true,
				Description: "A number from the last list, or a 12-character key prefix",
			}},
		},
		{
			Type: 1, Name: "login", Description: "Set a room server's password in a private popup",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "target",
				Description: "A number from the last list, or a key prefix. Defaults to this channel's room.",
			}},
		},
		{Type: 1, Name: "tidy", Description: "Drop links whose channel or mesh slot is gone"},
		{
			Type: 1, Name: "sync-rooms", Description: "Give every known room server a channel",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionBoolean, Name: "confirm",
				Description: "Actually do it — without this you get a count first",
			}},
		},
		{
			Type: 1, Name: "contact-add", Description: "Add a node to the radio from its full public key",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "key", Description: "The full 64-character public key", Required: true,
			}, {
				Type: discord.OptionString, Name: "type", Required: true,
				Description: "What it is — getting this wrong fails quietly, so it is not guessed",
				Choices: []discord.AppCommandChoice{
					{Name: "room server", Value: "room"},
					{Name: "companion (a person)", Value: "companion"},
					{Name: "repeater", Value: "repeater"},
					{Name: "sensor", Value: "sensor"},
				},
			}, {
				Type: discord.OptionString, Name: "name", Description: "What to call it", Required: true,
			}},
		},
		{
			Type: 1, Name: "contact-find", Description: "Search every contact on the radio",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "text", Description: "Leave empty to list them all",
			}},
		},
		{
			Type: 1, Name: "contact-info", Description: "Everything about one contact, including its full public key",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "target", Required: true,
				Description: "A number from the last contact-find, or a key prefix",
			}},
		},
		{
			Type: 1, Name: "contact-rename", Description: "Rename a contact on the radio",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "target", Required: true,
				Description: "A number from the last contact-find, or a key prefix",
			}, {
				Type: discord.OptionString, Name: "name", Description: "The new name", Required: true,
			}},
		},
		{
			Type: 1, Name: "contact-reset-path", Description: "Forget a contact's stored route so the next message floods",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "target", Required: true,
				Description: "A number from the last contact-find, or a key prefix",
			}},
		},
		{
			Type: 1, Name: "contact-type", Description: "Correct what a contact is (room server, companion, ...)",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "target", Required: true,
				Description: "A number from the last contact-find, or a key prefix",
			}, {
				Type: discord.OptionString, Name: "type", Description: "What it actually is", Required: true,
				Choices: []discord.AppCommandChoice{
					{Name: "room server", Value: "room"},
					{Name: "companion (a person)", Value: "companion"},
					{Name: "repeater", Value: "repeater"},
					{Name: "sensor", Value: "sensor"},
				},
			}},
		},
		{Type: 1, Name: "contact-refresh", Description: "Re-read the contact list from the radio"},
		{
			Type: 1, Name: "contact-remove", Description: "Delete a contact from the radio",
			Options: []discord.AppCommandOption{{
				Type: discord.OptionString, Name: "target", Required: true,
				Description: "A number from the last contact-find, or a full 64-character key",
			}},
		},
		{Type: 1, Name: "reset", Description: "Delete everything the bridge created (asks first)"},
	},
	// MANAGE_GUILD. Reconfiguring the bridge should not be available to
	// everyone who can type in the server.
	DefaultMemberPermissions: "32",
}}

// registerCommands publishes /mesh to the server.
//
// Guild-scoped, not global: guild commands appear immediately, while global
// ones can take an hour to propagate — which makes a fresh install look
// broken.
func (b *Bridge) registerCommands(ctx context.Context) {
	appID := b.cfg.ApplicationID()
	if appID == "" {
		app, err := b.rest.CurrentApplication(ctx)
		if err != nil {
			b.log.Warn("could not read the application id; slash commands are unavailable", "err", err)
			return
		}
		appID = app.ID
		_ = b.cfg.SetApplicationID(appID)
	}
	if err := b.rest.RegisterGuildCommands(ctx, appID, b.cfg.GuildID(), meshCommands); err != nil {
		b.log.Warn("could not register slash commands", "err", err)
		return
	}
	b.log.Info("registered /mesh slash commands")
}

// loginButtonRow builds the "Set password" button offered wherever a room
// server needs one.
func loginButtonRow(prefix, label string) discord.Component {
	text := "Set room password"
	if label != "" && label != prefix {
		text = "Set password for " + label
	}
	if len(text) > 80 {
		text = text[:80]
	}
	return discord.Component{
		Type: discord.ComponentActionRow,
		Components: []discord.Component{{
			Type:     discord.ComponentButton,
			Style:    discord.ButtonPrimary,
			Label:    text,
			CustomID: idLoginButton + prefix,
			Emoji:    &discord.Emoji{Name: "🔑"},
		}},
	}
}

// loginModal builds the private popup that collects a room password.
func loginModal(prefix, label string) discord.InteractionResponse {
	title := "Room server password"
	if label != "" && label != prefix {
		title = "Password for " + label
	}
	if len(title) > 45 {
		title = title[:45]
	}
	required := true
	return discord.InteractionResponse{
		Type: discord.RespondModal,
		Data: &discord.InteractionResponseData{
			CustomID: idLoginModal + prefix,
			Title:    title,
			Components: []discord.Component{{
				Type: discord.ComponentActionRow,
				Components: []discord.Component{{
					Type:        discord.ComponentTextInput,
					CustomID:    idLoginField,
					Style:       discord.TextInputShort,
					Label:       "Password",
					Placeholder: "The room server's password",
					MaxLength:   63, // the protocol truncates beyond this
					Required:    &required,
				}},
			}},
		},
	}
}

// ---------------------------------------------------------------------------
// Interaction dispatch
// ---------------------------------------------------------------------------

// onInteraction runs on the Gateway's read loop, so it hands the work
// straight to a goroutine.
//
// A goroutine each rather than a queue: Discord gives three seconds to
// acknowledge an interaction, and queueing one behind a split message being
// transmitted with two-second gaps would miss that every time. The semaphore
// keeps a burst of clicks from spawning without bound.
func (b *Bridge) onInteraction(i *discord.Interaction) {
	if i == nil {
		return
	}
	select {
	case b.interactionSlots <- struct{}{}:
	default:
		b.log.Warn("too many interactions at once; ignoring one", "type", i.Type)
		return
	}
	go func() {
		defer func() { <-b.interactionSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		switch i.Type {
		case discord.InteractionApplicationCommand:
			b.onSlashCommand(ctx, i)
		case discord.InteractionMessageComponent:
			b.onComponent(ctx, i)
		case discord.InteractionModalSubmit:
			b.onModalSubmit(ctx, i)
		}
	}()
}

// reply answers an interaction immediately, visible only to whoever ran it.
//
// Ephemeral by default: a listing of 200 contacts is for the person who asked,
// not for the channel.
func (b *Bridge) reply(ctx context.Context, i *discord.Interaction, text string, ephemeral bool) {
	data := &discord.InteractionResponseData{Content: clampReply(text)}
	if ephemeral {
		data.Flags = discord.MessageFlagEphemeral
	}
	if err := b.rest.RespondInteraction(ctx, i.ID, i.Token,
		discord.InteractionResponse{Type: discord.RespondMessage, Data: data}); err != nil {
		b.log.Warn("could not answer an interaction", "err", err)
	}
}

// deferReply buys time. An interaction must be answered within three seconds
// or Discord shows "the application did not respond", and creating a channel
// takes longer than that.
func (b *Bridge) deferReply(ctx context.Context, i *discord.Interaction, ephemeral bool) bool {
	data := &discord.InteractionResponseData{}
	if ephemeral {
		data.Flags = discord.MessageFlagEphemeral
	}
	err := b.rest.RespondInteraction(ctx, i.ID, i.Token,
		discord.InteractionResponse{Type: discord.RespondDeferMessage, Data: data})
	if err != nil {
		b.log.Warn("could not defer an interaction", "err", err)
		return false
	}
	return true
}

func (b *Bridge) finish(ctx context.Context, i *discord.Interaction, text string) {
	appID := b.cfg.ApplicationID()
	if appID == "" {
		return
	}
	if err := b.rest.EditInteractionResponse(ctx, appID, i.Token, clampReply(text)); err != nil {
		b.log.Warn("could not complete an interaction reply", "err", err)
	}
}

func (b *Bridge) onSlashCommand(ctx context.Context, i *discord.Interaction) {
	if i.Data.Name != "mesh" {
		return
	}
	sub, opts := i.Subcommand()
	actor := i.Actor().ID

	get := func(name string) discord.InteractionDataOption {
		for _, o := range opts {
			if o.Name == name {
				return o
			}
		}
		return discord.InteractionDataOption{}
	}

	switch sub {
	case "login":
		// A modal must be the FIRST response — it cannot follow a defer — so
		// everything this needs has to be resolvable from local state.
		b.openLoginModal(ctx, i, get("target").String())
		return

	case "reset":
		// Destructive enough to deserve a button rather than a typed word.
		b.confirmReset(ctx, i)
		return
	}

	// Everything else can take longer than three seconds, so defer first.
	if !b.deferReply(ctx, i, true) {
		return
	}

	var line string
	switch sub {
	case "status":
		line = "status"
	case "help":
		line = "help"
	case "tidy":
		line = "tidy"
	case "list":
		line = "list " + get("what").String()
		if get("unlinked").Bool() {
			line += " unlinked"
		}
		if s := get("sort").String(); s != "" {
			line += " " + s
		}
	case "find":
		line = "find " + get("text").String()
	case "link":
		line = "add " + get("target").String()
		if n := get("name").String(); n != "" {
			line += " as " + n
		}
	case "unlink":
		line = "remove " + get("target").String()
	case "sync-rooms":
		line = "sync rooms"
		if get("confirm").Bool() {
			line += " confirm"
		}
	case "contact-add":
		// Type before name: the name may contain spaces, the type may not.
		line = "contact add " + get("key").String() + " " + get("type").String() + " " + get("name").String()
	case "contact-find":
		line = strings.TrimSpace("contact find " + get("text").String())
	case "contact-remove":
		line = "contact remove " + get("target").String()
	case "contact-info":
		line = "contact info " + get("target").String()
	case "contact-rename":
		line = "contact rename " + get("target").String() + " " + get("name").String()
	case "contact-reset-path":
		line = "contact reset-path " + get("target").String()
	case "contact-refresh":
		line = "contact refresh"
	case "contact-type":
		line = "contact type " + get("target").String() + " " + get("type").String()
	default:
		b.finish(ctx, i, "Unknown subcommand.")
		return
	}

	reply := b.Exec(ctx, actor, line, i.ChannelID, "")
	if reply == "" {
		reply = "Done."
	}
	b.finish(ctx, i, reply)
}

// openLoginModal resolves which room is meant and pops up the password box.
func (b *Bridge) openLoginModal(ctx context.Context, i *discord.Interaction, target string) {
	prefix, label, err := b.resolveRoom(i.Actor().ID, i.ChannelID, target)
	if err != nil {
		b.reply(ctx, i, err.Error(), true)
		return
	}
	if err := b.rest.RespondInteraction(ctx, i.ID, i.Token, loginModal(prefix, label)); err != nil {
		b.log.Warn("could not open the password popup", "err", err)
	}
}

// resolveRoom works out which room server a login refers to: an explicit
// target, or the room this channel is linked to.
func (b *Bridge) resolveRoom(actor, channelID, target string) (prefix, label string, err error) {
	if strings.TrimSpace(target) == "" {
		r, rerr := b.db.RouteByChannel(channelID)
		if rerr != nil || r.Kind != store.KindRoom {
			return "", "", fmt.Errorf(
				"Which room server? Run this in a room server's own channel, or pass `target` — " +
					"a number from the last `/mesh list room servers`, or its 12-character key.")
		}
		return r.MeshKey, r.Label, nil
	}

	row, rerr := b.resolveTarget(actor, target)
	if rerr != nil {
		return "", "", rerr
	}
	if row.Kind != store.KindRoom {
		return "", "", fmt.Errorf(
			"**%s** is not a room server. Only room servers take a login — mesh channels use a "+
				"shared secret held on your node, and direct messages need no login at all.", row.Label)
	}
	return row.Key, row.Label, nil
}

func (b *Bridge) onComponent(ctx context.Context, i *discord.Interaction) {
	id := i.Data.CustomID

	switch {
	case strings.HasPrefix(id, idLoginButton):
		prefix := strings.TrimPrefix(id, idLoginButton)
		label := prefix
		if r, err := b.db.Route(store.KindRoom, prefix); err == nil && r.Label != "" {
			label = r.Label
		}
		if err := b.rest.RespondInteraction(ctx, i.ID, i.Token, loginModal(prefix, label)); err != nil {
			b.log.Warn("could not open the password popup", "err", err)
		}

	case id == idResetNo:
		b.reply(ctx, i, "Nothing was deleted.", true)

	case id == idResetYes:
		if !b.deferReply(ctx, i, true) {
			return
		}
		b.finish(ctx, i, b.Reset(ctx))
	}
}

func (b *Bridge) onModalSubmit(ctx context.Context, i *discord.Interaction) {
	id := i.Data.CustomID
	if !strings.HasPrefix(id, idLoginModal) {
		return
	}
	prefix := strings.TrimPrefix(id, idLoginModal)
	password := i.ModalValue(idLoginField)
	if strings.TrimSpace(password) == "" {
		b.reply(ctx, i, "No password was given, so nothing was changed.", true)
		return
	}

	label := prefix
	if r, err := b.db.Route(store.KindRoom, prefix); err == nil && r.Label != "" {
		label = r.Label
	}

	// Ephemeral, always. The whole point of the modal is that the password
	// never becomes visible to anyone else, and a public confirmation would
	// undo half of that by announcing who set it.
	if !b.deferReply(ctx, i, true) {
		return
	}
	b.finish(ctx, i, b.SetRoomPassword(ctx, prefix, label, password))
	b.db.LogEvent("info", "admin", "room password set for "+prefix+" via a private popup")
}

// confirmReset asks before deleting everything.
func (b *Bridge) confirmReset(ctx context.Context, i *discord.Interaction) {
	routes, _ := b.db.Routes()
	text := fmt.Sprintf(
		"This deletes **every channel the bridge created** — %d linked channel(s), the "+
			"**%s / %s / %s** categories, and **#%s** — and clears every link.\n"+
			"Channels you made yourself are untouched.",
		len(routes), CategoryChannels, CategoryRooms, CategoryDMs, InboxChannelName)

	err := b.rest.RespondInteraction(ctx, i.ID, i.Token, discord.InteractionResponse{
		Type: discord.RespondMessage,
		Data: &discord.InteractionResponseData{
			Content: text,
			Flags:   discord.MessageFlagEphemeral,
			Components: []discord.Component{{
				Type: discord.ComponentActionRow,
				Components: []discord.Component{
					{Type: discord.ComponentButton, Style: discord.ButtonDanger,
						Label: "Delete everything", CustomID: idResetYes},
					{Type: discord.ComponentButton, Style: discord.ButtonSecondary,
						Label: "Cancel", CustomID: idResetNo},
				},
			}},
		},
	})
	if err != nil {
		b.log.Warn("could not ask for reset confirmation", "err", err)
	}
}
