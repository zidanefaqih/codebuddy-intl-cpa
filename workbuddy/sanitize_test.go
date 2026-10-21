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

func TestSanitizeBlockedTemplates_DeepSeekHarness(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "verbatim",
			in:   "You are an AI agent powered by DeepSeek Harness. Follow the rules.",
			want: "You are an AI assistant. Follow the rules.",
		},
		{
			name: "no trailing period",
			in:   "You are an AI agent powered by DeepSeek Harness",
			want: "You are an AI assistant",
		},
		{
			name: "case drift",
			in:   "you are an AI agent powered by deepseek harness.",
			want: "You are an AI assistant.",
		},
		{
			name: "embedded in a long prompt",
			in:   "Intro line.\nYou are an AI agent powered by DeepSeek Harness.\nTools follow.",
			want: "Intro line.\nYou are an AI assistant.\nTools follow.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeBlockedTemplates(tc.in); got != tc.want {
				t.Fatalf("sanitizeBlockedTemplates(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Near-misses must survive untouched: only the full "You are an AI agent
// powered by DeepSeek Harness" sentence is blocklisted (verified by replay —
// each of these returned 200 against the live gateway).
func TestSanitizeBlockedTemplates_DeepSeekHarnessNearMisses(t *testing.T) {
	untouched := []string{
		"DeepSeek Harness",
		"powered by DeepSeek Harness",
		"You are an AI agent powered by DeepSeek.",
		"You are an AI agent powered by Acme Harness.",
		"AI agent powered by DeepSeek Harness.",
	}
	for _, in := range untouched {
		if got := sanitizeBlockedTemplates(in); got != in {
			t.Fatalf("sanitizeBlockedTemplates(%q) = %q, want it untouched", in, got)
		}
	}
}

// The rewrite must survive the whole request pipeline (system message inside a
// chat-completions body), not just the string helper.
func TestRewriteSystemForUpstream_DeepSeekHarness(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness. Be brief."},{"role":"user","content":"hi"}]}`)
	out := rewriteSystemForUpstream(in)
	if strings.Contains(string(out), "powered by DeepSeek Harness") {
		t.Fatalf("harness intro survived: %s", string(out))
	}
	if !strings.Contains(string(out), "You are an AI assistant. Be brief.") {
		t.Fatalf("rewrite lost the surrounding prompt: %s", string(out))
	}
}
