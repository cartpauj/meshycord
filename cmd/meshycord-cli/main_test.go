package main

import (
	"strings"
	"testing"
)

func TestBuildRequestParsesACommandAndTarget(t *testing.T) {
	req, timeout, err := buildRequest("clock sync", "", false, false, false, false, []string{"ridge-repeater"})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Op != "cli" || req.Target != "ridge-repeater" || req.Arg != "clock sync" {
		t.Errorf("request = %+v", req)
	}
	if timeout < 60_000_000_000 {
		t.Errorf("timeout = %s; a mesh round trip needs far longer", timeout)
	}
}

// Forgetting the target is the easy mistake, and it must not be read as
// "run it somewhere" — the command might be `reboot`.
func TestBuildRequestRefusesACommandWithNoTarget(t *testing.T) {
	_, _, err := buildRequest("reboot", "", false, false, false, false, nil)
	if err == nil {
		t.Fatal("a command with no target was accepted")
	}
	if !strings.Contains(err.Error(), "no target") {
		t.Errorf("err = %v", err)
	}
}

// An unquoted name with spaces arrives as several arguments. Sending the
// command to args[0] would pick a node nobody named.
func TestBuildRequestRefusesAnUnquotedMultiWordTarget(t *testing.T) {
	_, _, err := buildRequest("clock", "", false, false, false, false, []string{"ridge", "repeater"})
	if err == nil {
		t.Fatal("two targets were accepted")
	}
	if !strings.Contains(err.Error(), "quote") {
		t.Errorf("err = %v, want advice about quoting", err)
	}
}

func TestBuildRequestNeedsSomethingToDo(t *testing.T) {
	if _, _, err := buildRequest("", "", false, false, false, false, nil); err == nil {
		t.Error("an empty command line was accepted")
	}
}

func TestBuildRequestRefusesTwoJobsAtOnce(t *testing.T) {
	_, _, err := buildRequest("clock", "", true, false, false, false, []string{"ridge"})
	if err == nil {
		t.Error("-c and -list together were accepted")
	}
}

func TestBuildRequestLoginNeedsAPassword(t *testing.T) {
	if _, _, err := buildRequest("", "ridge", false, false, false, false, nil); err == nil {
		t.Error("a login with no password was accepted")
	}

	req, _, err := buildRequest("", "ridge", false, false, false, false, []string{"hunter2"})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Op != "login" || req.Target != "ridge" || req.Arg != "hunter2" {
		t.Errorf("request = %+v", req)
	}
}

func TestBuildRequestSimpleOperations(t *testing.T) {
	cases := []struct {
		name string
		call func() (string, error)
		want string
	}{
		{"list", func() (string, error) {
			r, _, err := buildRequest("", "", true, false, false, false, nil)
			return r.Op, err
		}, "list"},
		{"status", func() (string, error) {
			r, _, err := buildRequest("", "", false, true, false, false, nil)
			return r.Op, err
		}, "status"},
		{"clock", func() (string, error) {
			r, _, err := buildRequest("", "", false, false, true, false, nil)
			return r.Op, err
		}, "clock"},
		{"clock-sync", func() (string, error) {
			r, _, err := buildRequest("", "", false, false, false, true, nil)
			return r.Op, err
		}, "clock-sync"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.call()
			if err != nil {
				t.Fatalf("buildRequest: %v", err)
			}
			if got != c.want {
				t.Errorf("op = %q, want %q", got, c.want)
			}
		})
	}
}

// `status` is written for Discord and served to both. A terminal must not show
// literal ** and ``` fences.
func TestPlainTextStripsDiscordFormatting(t *testing.T) {
	in := "**Status**\n```\nmesh link   : DOWN\n```\nTry `meshycord-cli -list`."
	want := "Status\nmesh link   : DOWN\nTry meshycord-cli -list."
	if got := plainText(in); got != want {
		t.Errorf("plainText:\n got %q\nwant %q", got, want)
	}
}

// CLI output from a repeater is not Discord markup and must survive intact —
// backticks and asterisks in a config dump are data.
func TestPlainTextLeavesOrdinaryOutputAlone(t *testing.T) {
	in := "OK - clock set: 21:04 - 13/8/2026 UTC"
	if got := plainText(in); got != in {
		t.Errorf("plainText changed plain output: %q", got)
	}
}
