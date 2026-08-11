package discord

import (
	"encoding/json"
	"testing"
)

// The payload shape Discord actually sends when a button is pressed.
//
// data.id is an INTEGER here — components are numbered in Discord's newer
// schema — while for an application command the same field is a snowflake
// string. Typing it as a string made every button press fail to decode, so the
// bridge never answered and the user got "the application did not respond in
// time".
func TestButtonPressDecodes(t *testing.T) {
	raw := []byte(`{
	  "type": 3,
	  "id": "111111111111111111",
	  "token": "aW50ZXJhY3Rpb246MTIz",
	  "application_id": "222222222222222222",
	  "channel_id": "333333333333333333",
	  "guild_id": "555555555555555555",
	  "data": { "custom_id": "login:aabbccddeeff", "component_type": 2, "id": 2 },
	  "member": { "user": { "id": "42", "username": "cartpauj" } },
	  "message": { "id": "999", "channel_id": "333333333333333333", "content": "not logged in" }
	}`)

	var i Interaction
	if err := json.Unmarshal(raw, &i); err != nil {
		t.Fatalf("a real button press failed to decode: %v", err)
	}
	if i.Type != InteractionMessageComponent {
		t.Errorf("type = %d", i.Type)
	}
	if i.Data.CustomID != "login:aabbccddeeff" {
		t.Errorf("custom_id = %q — the bridge cannot tell which room it is for", i.Data.CustomID)
	}
	if i.ID == "" || i.Token == "" {
		t.Error("id/token missing; the interaction could not be answered at all")
	}
	if got := i.Actor().Username; got != "cartpauj" {
		t.Errorf("actor = %q", got)
	}
}

// A slash command sends the same field as a string. Both must work.
func TestSlashCommandDecodes(t *testing.T) {
	raw := []byte(`{
	  "type": 2,
	  "id": "111",
	  "token": "tok",
	  "channel_id": "222",
	  "data": {
	    "id": "444444444444444444",
	    "name": "mesh",
	    "type": 1,
	    "options": [{ "name": "login", "type": 1, "options": [
	      { "name": "target", "type": 3, "value": "aabbccddeeff" }
	    ]}]
	  },
	  "member": { "user": { "id": "42", "username": "cartpauj" } }
	}`)

	var i Interaction
	if err := json.Unmarshal(raw, &i); err != nil {
		t.Fatalf("a slash command failed to decode: %v", err)
	}
	sub, _ := i.Subcommand()
	if sub != "login" {
		t.Errorf("subcommand = %q", sub)
	}
	if got := i.OptString("target"); got != "aabbccddeeff" {
		t.Errorf("target = %q", got)
	}
}

// And a modal submission, which is how a room password comes back.
func TestModalSubmitDecodes(t *testing.T) {
	raw := []byte(`{
	  "type": 5,
	  "id": "333",
	  "token": "tok",
	  "channel_id": "222",
	  "data": {
	    "custom_id": "loginmodal:aabbccddeeff",
	    "components": [{ "type": 1, "id": 1, "components": [
	      { "type": 4, "id": 2, "custom_id": "password", "value": "hunter2" }
	    ]}]
	  },
	  "member": { "user": { "id": "42", "username": "cartpauj" } }
	}`)

	var i Interaction
	if err := json.Unmarshal(raw, &i); err != nil {
		t.Fatalf("a modal submission failed to decode: %v", err)
	}
	if got := i.ModalValue("password"); got != "hunter2" {
		t.Errorf("password = %q — the typed password never reached the bridge", got)
	}
}
