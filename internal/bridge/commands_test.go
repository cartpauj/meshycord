package bridge

import (
	"encoding/json"
	"strings"
	"testing"

	"meshycord/internal/discord"
)

// Discord parses emoji.id as a snowflake and rejects the WHOLE message with
// 400 if it is present but empty — which killed the button on the room-password
// prompt and, because the send error was swallowed, left the user with a bare
// ❌ and no explanation.
func TestButtonEmojiOmitsAnEmptyID(t *testing.T) {
	body, err := json.Marshal(discord.CreateMessage{
		Content:    "set a password",
		Components: []discord.Component{loginButtonRow("aabbccddeeff", "Ridge Room")},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), `"id":""`) {
		t.Errorf("empty emoji id is still being sent; Discord will 400 the whole message:\n%s", body)
	}
	if !strings.Contains(string(body), `"name":"🔑"`) {
		t.Errorf("the emoji itself went missing:\n%s", body)
	}
	// The parts Discord requires for a button must all survive.
	for _, want := range []string{`"type":1`, `"type":2`, `"custom_id":"login:aabbccddeeff"`, `"style":1`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("button JSON is missing %s:\n%s", want, body)
		}
	}
}

// The modal is the other half of that flow and must stay well-formed.
func TestLoginModalShape(t *testing.T) {
	body, err := json.Marshal(loginModal("aabbccddeeff", "Ridge Room"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		`"type":9`,                       // MODAL
		`"custom_id":"loginmodal:aabbcc`, // carries the room it is for
		`"type":4`,                       // a text input
		`"custom_id":"password"`,
		`"required":true`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("modal JSON is missing %s:\n%s", want, s)
		}
	}
	// A modal title is capped at 45 characters by Discord.
	var probe struct {
		Data struct {
			Title string `json:"title"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &probe)
	if len(probe.Data.Title) > 45 {
		t.Errorf("modal title is %d characters, over Discord's 45 limit", len(probe.Data.Title))
	}
}
