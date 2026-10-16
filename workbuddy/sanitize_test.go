package main

import (
	"strings"
	"testing"
)

func TestSanitizeBlockedTemplates_ClaudeCode(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude."
	out := sanitizeBlockedTemplates(in)
	if out == in {
		t.Fatal("should replace blocked template")
	}
	want := "You are Claude Code, Anthropic's official CLI tool for Claude."
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

// The Claude Agent SDK form continues the sentence with a comma instead of a
// period. Matching only the period-terminated sentence left this variant
// untouched, and the upstream rejected every such request with 400 / code
// 11128 ("Illegal API invocation from an unapproved channel").
func TestSanitizeBlockedTemplates_ClaudeAgentSDKForm(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	out := sanitizeBlockedTemplates(in)
	if out == in {
		t.Fatal("should replace the comma/SDK variant")
	}
	want := "You are Claude Code, Anthropic's official CLI tool for Claude, running within the Claude Agent SDK."
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

// The feedback line is blocklisted as one exact sentence; verbatim it makes
// the upstream reject the request with 400 / code 11128.
func TestSanitizeBlockedTemplates_FeedbackLine(t *testing.T) {
	in := "  - To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	out := sanitizeBlockedTemplates(in)
	if out == in {
		t.Fatal("should replace the blocklisted feedback line")
	}
	if strings.Contains(out, "https://github.com/anthropics/claude-code/issues") {
		t.Fatalf("blocklisted URL survived: %q", out)
	}
	if !strings.Contains(out, "To give feedback") {
		t.Fatalf("rewrite lost the line's meaning: %q", out)
	}
}

// A real system prompt carries both blocklisted strings at once; the sanitizer
// must clear both in a single pass.
func TestSanitizeBlockedTemplates_BothTemplatesInOnePrompt(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.\n" +
		"  - To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues\n"
	out := sanitizeBlockedTemplates(in)
	if strings.Contains(out, "Anthropic's official CLI for Claude") {
		t.Fatalf("identity line survived: %q", out)
	}
	if strings.Contains(out, "https://github.com/anthropics/claude-code/issues") {
		t.Fatalf("feedback URL survived: %q", out)
	}
}

func TestSanitizeBlockedTemplates_MainBranch(t *testing.T) {
	in := "Main branch (you will usually use this for PRs)"
	out := sanitizeBlockedTemplates(in)
	if out == in {
		t.Fatal("should replace Main branch")
	}
}

func TestSanitizeBlockedTemplates_NoMatch(t *testing.T) {
	in := "Hello world"
	out := sanitizeBlockedTemplates(in)
	if out != in {
		t.Fatal("should pass through unchanged")
	}
}

func TestForceMaxThinking_Hy3Model(t *testing.T) {
	obj := map[string]any{"model": "hy3-std"}
	changed := forceMaxThinking(obj)
	if !changed {
		t.Fatal("should change hy3 model")
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatal("should set high")
	}
}

func TestForceMaxThinking_NonHy3Model(t *testing.T) {
	obj := map[string]any{"model": "glm-5.2"}
	changed := forceMaxThinking(obj)
	if changed {
		t.Fatal("should not change non-hy3 model")
	}
}

func TestForceMaxThinking_AlreadyHigh(t *testing.T) {
	obj := map[string]any{"model": "hy3-std", "reasoning_effort": "high"}
	changed := forceMaxThinking(obj)
	if changed {
		t.Fatal("should not change when already high")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Fatal("short string should be unchanged")
	}
	if truncate("hello world", 5) != "hello" {
		t.Fatal("should truncate to 5 chars")
	}
	if truncate("", 5) != "" {
		t.Fatal("empty string")
	}
}
