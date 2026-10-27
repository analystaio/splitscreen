package main

import (
	"strings"
	"testing"
)

func TestRepoFromPath(t *testing.T) {
	// Git supplies this differently depending on the remote URL; the gateway
	// matches forge policy against the normalized form, so every shape has to
	// land on the same answer.
	tests := map[string]string{
		"acme/widgets":      "acme/widgets",
		"/acme/widgets":     "acme/widgets",
		"acme/widgets.git":  "acme/widgets",
		"/acme/widgets.git": "acme/widgets",
		" acme/widgets ":    "acme/widgets",
		"":                  "",
		"/":                 "",
	}
	for in, want := range tests {
		if got := repoFromPath(in); got != want {
			t.Errorf("repoFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadCredentialAttrs(t *testing.T) {
	// Git writes key=value lines terminated by a blank line.
	input := "protocol=https\nhost=github.com\npath=acme/widgets.git\n\n"
	attrs, err := readCredentialAttrs(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if attrs["host"] != "github.com" || attrs["path"] != "acme/widgets.git" {
		t.Fatalf("attrs = %+v", attrs)
	}
	if attrs["protocol"] != "https" {
		t.Errorf("protocol = %q", attrs["protocol"])
	}
}

func TestReadCredentialAttrsStopsAtBlankLine(t *testing.T) {
	// Anything after the blank terminator belongs to the next request and must
	// not bleed into this one.
	attrs, err := readCredentialAttrs(strings.NewReader("path=a/b\n\npath=other/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	if attrs["path"] != "a/b" {
		t.Fatalf("path = %q; input past the terminator leaked in", attrs["path"])
	}
}

// Every subcommand must be reachable, and the helpers the runner spawns must
// stay hidden from the top-level listing.
func TestCommandTree(t *testing.T) {
	cmds := map[string]bool{}
	hidden := map[string]bool{}
	for _, c := range allCommands() {
		cmds[c.Name()] = true
		hidden[c.Name()] = c.Hidden
	}
	for _, want := range []string{"gateway", "runner", "enroll", "config", "cert",
		"credential-helper", "permission-shim", "mcp-shim", "send-file", "usage"} {
		if !cmds[want] {
			t.Errorf("missing subcommand %q", want)
		}
	}
	for _, shim := range []string{"permission-shim", "mcp-shim"} {
		if !hidden[shim] {
			t.Errorf("%q should be hidden; it is spawned by the runner, not typed by a human", shim)
		}
	}
	if hidden["send-file"] {
		t.Error("send-file is meant to be called from an agent's shell; it should be listed")
	}
}
