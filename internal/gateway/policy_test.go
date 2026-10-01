package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/analystaio/splitscreen/config"
)

func TestMatchDeny(t *testing.T) {
	rules := []string{
		"Bash(git push --force*)",
		"Bash(rm -rf /*)",
		"WebFetch",
		"Edit(/etc/*)",
	}

	tests := []struct {
		name  string
		tool  string
		input string
		deny  bool
	}{
		{"force push denied", "Bash", `{"command":"git push --force origin main"}`, true},
		{"ordinary push allowed", "Bash", `{"command":"git push origin main"}`, false},
		{"whole tool denied", "WebFetch", `{"url":"https://example.com"}`, true},
		{"path rule denied", "Edit", `{"file_path":"/etc/passwd"}`, true},
		{"path rule not matched", "Edit", `{"file_path":"/srv/app/main.go"}`, false},
		{"unrelated tool allowed", "Read", `{"file_path":"/etc/passwd"}`, false},
		{"rm -rf denied", "Bash", `{"command":"rm -rf /var/www"}`, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rule, denied := MatchDeny(rules, tc.tool, json.RawMessage(tc.input))
			if denied != tc.deny {
				t.Fatalf("denied = %v (rule %q), want %v", denied, rule, tc.deny)
			}
		})
	}
}

// A rule must not be evadable by burying the argument somewhere the matcher
// does not look; when no recognized field exists, the whole payload is matched
// so a rule still has something to bite on.
func TestSalientArgFallsBackToWholePayload(t *testing.T) {
	rules := []string{"Weird(*dangerous*)"}
	if _, denied := MatchDeny(rules, "Weird", json.RawMessage(`{"unexpected":"dangerous thing"}`)); !denied {
		t.Fatal("expected the rule to match against the whole payload")
	}
}

func TestMatchDenyEmptyRules(t *testing.T) {
	if _, denied := MatchDeny(nil, "Bash", json.RawMessage(`{"command":"anything"}`)); denied {
		t.Fatal("no rules must mean no denial")
	}
}

func TestRepoAllowed(t *testing.T) {
	tests := []struct {
		allowed []string
		repo    string
		want    bool
	}{
		{[]string{"acme/widgets"}, "acme/widgets", true},
		{[]string{"acme/widgets"}, "acme/other", false},
		{[]string{"acme/*"}, "acme/anything", true},
		{[]string{"acme/*"}, "evil/anything", false},
		{[]string{"Acme/Widgets"}, "acme/widgets", true}, // forge names are case-insensitive
		// An empty allowlist denies everything: a runner with no declared
		// repositories has no business minting git credentials.
		{nil, "acme/widgets", false},
		{[]string{}, "acme/widgets", false},
	}
	for _, tc := range tests {
		if got := RepoAllowed(tc.allowed, tc.repo); got != tc.want {
			t.Errorf("RepoAllowed(%v, %q) = %v, want %v", tc.allowed, tc.repo, got, tc.want)
		}
	}
}

func TestSummarizeInput(t *testing.T) {
	if got := SummarizeInput(json.RawMessage(`{"command":"ls -la"}`)); got != "ls -la" {
		t.Errorf("summary = %q", got)
	}
	if got := SummarizeInput(nil); got != "" {
		t.Errorf("summary of nothing = %q, want empty", got)
	}
}

func TestAutoApprove(t *testing.T) {
	if _, auto := autoApprove(&config.Runner{}, "Bash", nil); auto {
		t.Error("a runner with no allow rules and no auto_approve should prompt")
	}

	withAllow := &config.Runner{Policy: config.Policy{Allow: []string{"Read", "Bash(git status*)"}}}
	if _, auto := autoApprove(withAllow, "Read", nil); !auto {
		t.Error("an allow-listed tool should not prompt")
	}
	if _, auto := autoApprove(withAllow, "Bash", json.RawMessage(`{"command":"git status"}`)); !auto {
		t.Error("an allow rule with an argument pattern should match")
	}
	if _, auto := autoApprove(withAllow, "Bash", json.RawMessage(`{"command":"rm -rf /"}`)); auto {
		t.Error("a non-matching command was auto-approved")
	}

	unattended := &config.Runner{Policy: config.Policy{AutoApprove: true}}
	reason, auto := autoApprove(unattended, "Anything", nil)
	if !auto {
		t.Fatal("an unattended runner should not prompt")
	}
	if !strings.Contains(reason, "unattended") {
		t.Errorf("reason = %q; it should record why no human was asked", reason)
	}
}

func TestPostureText(t *testing.T) {
	unattended := &config.Runner{Policy: config.Policy{
		AutoApprove: true, Deny: []string{"a", "b"},
	}}
	got := PostureText(unattended)
	if !strings.Contains(got, "unattended") || !strings.Contains(got, "2 deny") {
		t.Errorf("posture = %q", got)
	}

	// Unattended with nothing denied is the one combination that deserves to
	// look alarming in status output.
	bare := &config.Runner{Policy: config.Policy{AutoApprove: true}}
	if !strings.Contains(PostureText(bare), "no deny rules") {
		t.Errorf("posture = %q; an unguarded unattended runner must be visible", PostureText(bare))
	}
}

func TestSessionGrants(t *testing.T) {
	g := newGrantStore()
	if _, ok := g.Held("t1", "Bash"); ok {
		t.Fatal("an ungranted tool was reported as held")
	}

	g.Grant("t1", "Bash", "U1")
	gr, ok := g.Held("t1", "Bash")
	if !ok || gr.By != "U1" {
		t.Fatalf("grant = %+v ok=%v", gr, ok)
	}
	// Grants are per thread: approving in one conversation must not leak into
	// another.
	if _, ok := g.Held("t2", "Bash"); ok {
		t.Fatal("a grant leaked across threads")
	}
	if _, ok := g.Held("t1", "Write"); ok {
		t.Fatal("a grant leaked across tools")
	}

	if n := g.Clear("t1"); n != 1 {
		t.Fatalf("cleared %d grants, want 1", n)
	}
	if _, ok := g.Held("t1", "Bash"); ok {
		t.Fatal("a grant survived the session it was scoped to")
	}
}
