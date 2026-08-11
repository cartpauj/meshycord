package discord

import "testing"

// An interaction token is unique per interaction. Leaving it in the rate-limit
// bucket key mints a new bucket every time and grows the map without bound.
func TestBucketKeyCollapsesUniqueTokens(t *testing.T) {
	a := bucketKey("POST", "/interactions/111111111111111111/aW50ZXJhY3Rpb246MTIz/callback")
	b := bucketKey("POST", "/interactions/9999999999999999999/dGhpc0lzQURpZmZlcmVudA/callback")
	if a != b {
		t.Errorf("two interactions produced different buckets:\n  %s\n  %s", a, b)
	}
	if want := "POST /interactions/{id}/{token}/callback"; a != want {
		t.Errorf("bucket = %q, want %q", a, want)
	}

	// Webhook tokens too — that is the "edit my deferred reply" route.
	w := bucketKey("PATCH", "/webhooks/222222222222222222/sometoken/messages/@original")
	if want := "PATCH /webhooks/222222222222222222/{token}/messages/@original"; w != want {
		t.Errorf("webhook bucket = %q, want %q", w, want)
	}
}

// Discord's limits really are per channel and per guild, so those ids must
// survive or every channel would share one bucket and serialise behind it.
func TestBucketKeyKeepsMajorParameters(t *testing.T) {
	a := bucketKey("POST", "/channels/111/messages")
	b := bucketKey("POST", "/channels/222/messages")
	if a == b {
		t.Error("two different channels collapsed into one rate-limit bucket")
	}

	// A message id is not a major parameter and should collapse, so reacting
	// to many messages in a channel shares that channel's bucket.
	x := bucketKey("PUT", "/channels/111/messages/333/reactions/x/@me")
	y := bucketKey("PUT", "/channels/111/messages/444/reactions/x/@me")
	if x != y {
		t.Errorf("message ids did not collapse:\n  %s\n  %s", x, y)
	}
}
