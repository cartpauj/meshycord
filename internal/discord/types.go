// Package discord is a hand-written Discord client: REST on net/http and the
// Gateway directly on a websocket.
//
// No library, deliberately. The ESP32 version hand-wrote REST and that part
// was never the problem — the standard library gives TLS, keep-alive,
// connection pooling and JSON for free, so what is left is small enough to
// read in one sitting and carries no dependency risk. The one thing Go's
// standard library lacks is a websocket, which is why coder/websocket is here.
package discord

import "encoding/json"

// APIBase is the versioned REST root. v10 is current.
const APIBase = "https://discord.com/api/v10"

// Channel types. Only two matter here.
const (
	ChannelTypeText     = 0
	ChannelTypeCategory = 4
)

// Gateway intents.
//
// MessageContent is a privileged intent and must be turned ON in the
// Developer Portal. Without it every message arrives with an empty content
// field, which presents as "the bot sees messages but they are all blank".
const (
	IntentGuilds                = 1 << 0
	IntentGuildMessages         = 1 << 9
	IntentGuildMessageReactions = 1 << 10
	IntentMessageContent        = 1 << 15

	// Intents is what the bridge asks for: guild structure (so a deleted
	// channel is noticed), messages, their content, and reactions.
	//
	// Reactions are the headline. They are delivered ONLY over the Gateway —
	// there is no REST endpoint to poll for them — which is the single
	// capability the ESP32 version could never have.
	Intents = IntentGuilds | IntentGuildMessages | IntentGuildMessageReactions | IntentMessageContent
)

// User is a Discord account or bot.
type User struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	GlobalName    string `json:"global_name"`
	Discriminator string `json:"discriminator"`
	Bot           bool   `json:"bot"`
}

// Display picks the nicest available name.
func (u User) Display() string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// Member is a user as seen inside a guild.
type Member struct {
	User *User  `json:"user"`
	Nick string `json:"nick"`
}

// Channel is a text channel or a category.
type Channel struct {
	ID       string `json:"id"`
	Type     int    `json:"type"`
	GuildID  string `json:"guild_id"`
	Name     string `json:"name"`
	Topic    string `json:"topic"`
	ParentID string `json:"parent_id"`
	Position int    `json:"position"`
}

// MessageReference names the message a reply points at.
type MessageReference struct {
	MessageID string `json:"message_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	GuildID   string `json:"guild_id,omitempty"`
}

// Emoji identifies a reaction or decorates a button.
//
// ID must be omitted entirely for a unicode emoji — Discord parses it as a
// snowflake and rejects the whole request with 400 "Value \"\" is not
// snowflake" if it is present but empty. That failure took out the button on
// the room-password prompt, and because the send error was swallowed the user
// saw a bare ❌ with no explanation at all.
type Emoji struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

// Reaction is a reaction summary attached to a fetched message.
type Reaction struct {
	Count int   `json:"count"`
	Me    bool  `json:"me"`
	Emoji Emoji `json:"emoji"`
}

// Message is a Discord message.
type Message struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Content   string `json:"content"`
	Author    User   `json:"author"`
	WebhookID string `json:"webhook_id"`
	Type      int    `json:"type"`

	// MessageReference always names the message a reply refers to.
	MessageReference *MessageReference `json:"message_reference"`
	// ReferencedMessage is usually inlined for replies (type 19), which saves
	// a fetch — but Discord does not promise it, so a nil here means "go and
	// get it", not "this was not a reply".
	ReferencedMessage *Message `json:"referenced_message"`

	Reactions []Reaction `json:"reactions"`
}

// IsFromBot reports whether this message came from a bot or a webhook.
//
// The echo-loop guard. Never surface our own posts, or any other bot's, as
// something to send to the mesh. Getting this wrong floods the radio.
func (m Message) IsFromBot() bool { return m.Author.Bot || m.WebhookID != "" }

// ---------------------------------------------------------------------------
// Interactions — what the Gateway unlocks
// ---------------------------------------------------------------------------

// Interaction types.
const (
	InteractionPing               = 1
	InteractionApplicationCommand = 2
	InteractionMessageComponent   = 3
	InteractionAutocomplete       = 4
	InteractionModalSubmit        = 5
)

// Interaction response types.
const (
	RespondPong               = 1
	RespondMessage            = 4 // reply with a message
	RespondDeferMessage       = 5 // "thinking…", then edit in the real answer
	RespondDeferUpdate        = 6
	RespondUpdateMessage      = 7
	RespondAutocompleteResult = 8
	RespondModal              = 9 // a private popup text box
)

// MessageFlagEphemeral makes a reply visible only to whoever triggered it.
const MessageFlagEphemeral = 1 << 6

// Component types.
const (
	ComponentActionRow  = 1
	ComponentButton     = 2
	ComponentStringMenu = 3
	ComponentTextInput  = 4
)

// Button styles.
const (
	ButtonPrimary   = 1
	ButtonSecondary = 2
	ButtonSuccess   = 3
	ButtonDanger    = 4
	ButtonLink      = 5
)

// Text input styles.
const (
	TextInputShort     = 1
	TextInputParagraph = 2
)

// Component is one interactive element. The same shape covers action rows,
// buttons, select menus and text inputs, which is Discord's design rather
// than ours.
type Component struct {
	Type        int         `json:"type"`
	CustomID    string      `json:"custom_id,omitempty"`
	Style       int         `json:"style,omitempty"`
	Label       string      `json:"label,omitempty"`
	Placeholder string      `json:"placeholder,omitempty"`
	Value       string      `json:"value,omitempty"`
	MinLength   *int        `json:"min_length,omitempty"`
	MaxLength   int         `json:"max_length,omitempty"`
	Required    *bool       `json:"required,omitempty"`
	Emoji       *Emoji      `json:"emoji,omitempty"`
	URL         string      `json:"url,omitempty"`
	Disabled    bool        `json:"disabled,omitempty"`
	Options     []SelectOpt `json:"options,omitempty"`
	Components  []Component `json:"components,omitempty"`
}

// SelectOpt is one entry in a select menu.
type SelectOpt struct {
	Label       string `json:"label"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

// Application command option types.
const (
	OptionString  = 3
	OptionInteger = 4
	OptionBoolean = 5
)

// AppCommand is a slash command definition.
type AppCommand struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Options     []AppCommandOption `json:"options,omitempty"`
	// DefaultMemberPermissions restricts who may run it. "32" is
	// MANAGE_GUILD: commands that reconfigure the bridge should not be
	// available to everyone who can type in the server.
	DefaultMemberPermissions string `json:"default_member_permissions,omitempty"`
}

// AppCommandOption is one argument of a slash command.
type AppCommandOption struct {
	Type         int                `json:"type"`
	Name         string             `json:"name"`
	Description  string             `json:"description"`
	Required     bool               `json:"required,omitempty"`
	Autocomplete bool               `json:"autocomplete,omitempty"`
	Choices      []AppCommandChoice `json:"choices,omitempty"`
	Options      []AppCommandOption `json:"options,omitempty"`
}

// AppCommandChoice is a fixed choice for an option.
type AppCommandChoice struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// InteractionData is the payload of a command, component or modal submission.
type InteractionData struct {
	// ID is a snowflake STRING for an application command but an integer for a
	// message component — Discord's newer component schema numbers components.
	// Typing it as a string made every button press fail to decode, so the
	// bridge never answered and Discord told the user the application had not
	// responded in time.
	//
	// Raw because nothing here reads it; the point is only that it must not
	// break the decode of everything around it.
	ID       json.RawMessage         `json:"id"`
	Name     string                  `json:"name"`
	CustomID string                  `json:"custom_id"`
	Type     int                     `json:"type"`
	Options  []InteractionDataOption `json:"options"`
	// Components carries submitted modal fields.
	Components []Component `json:"components"`
	Values     []string    `json:"values"`
}

// InteractionDataOption is one supplied argument.
type InteractionDataOption struct {
	Name    string                  `json:"name"`
	Type    int                     `json:"type"`
	Value   json.RawMessage         `json:"value"`
	Focused bool                    `json:"focused"`
	Options []InteractionDataOption `json:"options"`
}

// String reads an option's value as text.
func (o InteractionDataOption) String() string {
	var s string
	if err := json.Unmarshal(o.Value, &s); err == nil {
		return s
	}
	var n float64
	if err := json.Unmarshal(o.Value, &n); err == nil {
		return trimFloat(n)
	}
	return ""
}

// Int reads an option's value as a whole number.
func (o InteractionDataOption) Int() int {
	var n int
	if err := json.Unmarshal(o.Value, &n); err == nil {
		return n
	}
	return 0
}

// Bool reads an option's value as a flag.
func (o InteractionDataOption) Bool() bool {
	var b bool
	_ = json.Unmarshal(o.Value, &b)
	return b
}

// Interaction is a slash command, button press or modal submission.
type Interaction struct {
	ID        string          `json:"id"`
	Token     string          `json:"token"`
	Type      int             `json:"type"`
	GuildID   string          `json:"guild_id"`
	ChannelID string          `json:"channel_id"`
	Member    *Member         `json:"member"`
	User      *User           `json:"user"`
	Data      InteractionData `json:"data"`
	Message   *Message        `json:"message"`
}

// Actor names whoever triggered the interaction, wherever Discord put them.
func (i Interaction) Actor() User {
	if i.Member != nil && i.Member.User != nil {
		return *i.Member.User
	}
	if i.User != nil {
		return *i.User
	}
	return User{}
}

// Option finds a supplied argument by name.
func (i Interaction) Option(name string) (InteractionDataOption, bool) {
	for _, o := range i.Data.Options {
		if o.Name == name {
			return o, true
		}
		for _, sub := range o.Options {
			if sub.Name == name {
				return sub, true
			}
		}
	}
	return InteractionDataOption{}, false
}

// OptString reads a string argument, or "".
func (i Interaction) OptString(name string) string {
	o, ok := i.Option(name)
	if !ok {
		return ""
	}
	return o.String()
}

// Subcommand returns the name of the invoked subcommand, if any.
func (i Interaction) Subcommand() (string, []InteractionDataOption) {
	for _, o := range i.Data.Options {
		// 1 is SUB_COMMAND, 2 is SUB_COMMAND_GROUP.
		if o.Type == 1 || o.Type == 2 {
			return o.Name, o.Options
		}
	}
	return "", i.Data.Options
}

// ModalValue pulls a submitted field out of a modal payload. Discord nests
// every input inside its own action row.
func (i Interaction) ModalValue(customID string) string {
	for _, row := range i.Data.Components {
		for _, c := range row.Components {
			if c.CustomID == customID {
				return c.Value
			}
		}
		if row.CustomID == customID {
			return row.Value
		}
	}
	return ""
}

// InteractionResponse is what we send back.
type InteractionResponse struct {
	Type int                      `json:"type"`
	Data *InteractionResponseData `json:"data,omitempty"`
}

// InteractionResponseData is the body of a response, a modal, or an edit.
type InteractionResponseData struct {
	Content    string             `json:"content,omitempty"`
	Flags      int                `json:"flags,omitempty"`
	CustomID   string             `json:"custom_id,omitempty"`
	Title      string             `json:"title,omitempty"`
	Components []Component        `json:"components,omitempty"`
	Choices    []AppCommandChoice `json:"choices,omitempty"`
}

// ---------------------------------------------------------------------------
// Gateway payloads
// ---------------------------------------------------------------------------

// Gateway opcodes.
const (
	OpDispatch            = 0
	OpHeartbeat           = 1
	OpIdentify            = 2
	OpPresenceUpdate      = 3
	OpVoiceStateUpdate    = 4
	OpResume              = 6
	OpReconnect           = 7
	OpRequestGuildMembers = 8
	OpInvalidSession      = 9
	OpHello               = 10
	OpHeartbeatACK        = 11
)

// gatewayPayload is the envelope every Gateway frame arrives in.
type gatewayPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int            `json:"s"`
	T  string          `json:"t"`
}

type helloData struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}

type readyData struct {
	SessionID        string `json:"session_id"`
	ResumeGatewayURL string `json:"resume_gateway_url"`
	User             User   `json:"user"`
	Application      struct {
		ID string `json:"id"`
	} `json:"application"`
}

type identifyProperties struct {
	OS      string `json:"os"`
	Browser string `json:"browser"`
	Device  string `json:"device"`
}

type identifyData struct {
	Token      string             `json:"token"`
	Intents    int                `json:"intents"`
	Properties identifyProperties `json:"properties"`
}

type resumeData struct {
	Token     string `json:"token"`
	SessionID string `json:"session_id"`
	Seq       int    `json:"seq"`
}

// ReactionEvent is MESSAGE_REACTION_ADD / _REMOVE.
type ReactionEvent struct {
	UserID    string  `json:"user_id"`
	ChannelID string  `json:"channel_id"`
	MessageID string  `json:"message_id"`
	GuildID   string  `json:"guild_id"`
	Emoji     Emoji   `json:"emoji"`
	Member    *Member `json:"member"`
	// Added distinguishes ADD from REMOVE; filled in by the dispatcher, not
	// by Discord.
	Added bool `json:"-"`
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return itoa(int64(f))
	}
	b, _ := json.Marshal(f)
	return string(b)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
