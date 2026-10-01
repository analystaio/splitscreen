package gateway

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/analystaio/splitscreen/config"
)

// MatchDeny reports whether any rule denies a tool invocation, and which rule
// did.
//
// Rules take two forms:
//
//	Bash                        — deny the tool outright
//	Bash(git push --force*)     — deny when the salient argument matches a glob
//
// Evaluation happens on the gateway, before a prompt is posted, so a denial
// cannot be overridden by clicking Allow. That inversion is the point: file
// contents and third-party API responses are untrusted input the agent reads as
// instructions, so the agent's judgment cannot be the control.
func MatchDeny(rules []string, tool string, input json.RawMessage) (string, bool) {
	arg := salientArg(input)
	for _, rule := range rules {
		if matchRule(rule, tool, arg) {
			return rule, true
		}
	}
	return "", false
}

func matchRule(rule, tool, arg string) bool {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return false
	}
	name, pattern, hasArgs := cutRule(rule)
	if !globMatch(name, tool) {
		return false
	}
	if !hasArgs {
		return true
	}
	return globMatch(pattern, arg)
}

// cutRule splits "Tool(pattern)" into its parts.
func cutRule(rule string) (name, pattern string, hasArgs bool) {
	open := strings.Index(rule, "(")
	if open < 0 || !strings.HasSuffix(rule, ")") {
		return rule, "", false
	}
	return strings.TrimSpace(rule[:open]), rule[open+1 : len(rule)-1], true
}

// globMatch supports * wildcards anywhere in the pattern. path.Match is used for
// its glob semantics, with a prefix fast path for the common trailing-* case;
// path.Match treats "/" as a separator, so a bare prefix check is also tried to
// keep rules like "Bash(git push*)" working against multi-segment arguments.
func globMatch(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	if pattern == "*" {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	if ok, err := path.Match(pattern, s); err == nil && ok {
		return true
	}
	return pattern == s
}

// salientArg picks the field of a tool input that a human would recognize as
// "what it is about". Rules are written against this, so the choice is part of
// the contract rather than an implementation detail.
func salientArg(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return string(input)
	}
	for _, key := range []string{"command", "file_path", "path", "url", "pattern", "query"} {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	// No recognized field: match against the whole payload so a rule can still
	// be written, rather than silently matching nothing.
	return string(input)
}

// SummarizeInput renders a tool input for a permission prompt.
func SummarizeInput(input json.RawMessage) string {
	arg := salientArg(input)
	if arg == "" {
		return ""
	}
	return arg
}

// autoApprove reports whether static policy lets a tool call proceed without
// asking a human, and why.
//
// It deliberately does not consult deny rules — the caller evaluates those
// first. Keeping the two apart means no allow rule can ever be written that
// quietly overrides a denial.
func autoApprove(rc *config.Runner, tool string, input json.RawMessage) (string, bool) {
	if rc == nil {
		return "", false
	}
	if rule, matched := MatchDeny(rc.Policy.Allow, tool, input); matched {
		return "auto-approved by allow rule: " + rule, true
	}
	if rc.Policy.AutoApprove {
		return "auto-approved: this runner runs unattended", true
	}
	return "", false
}

// PostureText summarizes how a runner handles permission requests, so "why
// didn't it ask me?" has an answer somebody can look up.
func PostureText(rc *config.Runner) string {
	if rc == nil {
		return "unknown"
	}
	var parts []string
	if rc.Policy.AutoApprove {
		parts = append(parts, "unattended")
	} else {
		parts = append(parts, "prompting")
	}
	if n := len(rc.Policy.Allow); n > 0 {
		parts = append(parts, fmt.Sprintf("%d allow", n))
	}
	if n := len(rc.Policy.Deny); n > 0 {
		parts = append(parts, fmt.Sprintf("%d deny", n))
	} else if rc.Policy.AutoApprove {
		parts = append(parts, "*no deny rules*")
	}
	if n := len(rc.Policy.Approvers); n > 0 && !rc.Policy.AutoApprove {
		parts = append(parts, fmt.Sprintf("%d approvers", n))
	}
	return strings.Join(parts, " · ")
}

// RepoAllowed reports whether a runner's forge policy permits a repository.
// An empty list denies everything: a runner with no declared repositories has
// no business minting git credentials.
func RepoAllowed(allowed []string, repo string) bool {
	for _, a := range allowed {
		if strings.EqualFold(a, repo) {
			return true
		}
		if prefix, ok := strings.CutSuffix(a, "/*"); ok {
			if owner, _, found := strings.Cut(repo, "/"); found && strings.EqualFold(owner, prefix) {
				return true
			}
		}
	}
	return false
}
