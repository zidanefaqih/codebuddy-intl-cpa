package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The channel gate is content AND size: pure filler of the same length passes,
// a real agent prompt past ~62 KB does not. Detection must key on the upstream
// error shape, not on the request.
func TestUpstreamBlocked11128(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"business code", 400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, true},
		{"string code", 400, `{"code":"11128"}`, true},
		{"phrase only", 400, `{"msg":"Illegal API invocation from an unapproved channel"}`, true},
		{"unapproved only", 400, `{"displayMsg":{"en":"Request blocked by an unapproved channel"}}`, true},
		{"other 400", 400, `{"error":"bad request"}`, false},
		{"429 is not the gate", 429, `{"code":11128}`, false},
		{"500 is not the gate", 500, `{"code":11128}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := upstreamBlocked11128(c.status, c.body); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

// Folding must move the system prompt into the first user turn and leave no
// system message behind, because the gate scans the system role specifically.
func TestFoldSystemIntoUser(t *testing.T) {
	in := `{"model":"m","messages":[
		{"role":"system","content":"SYS PROMPT"},
		{"role":"user","content":"hello"},
		{"role":"assistant","content":"hi"}
	]}`
	out, ok := foldSystemIntoUser([]byte(in))
	if !ok {
		t.Fatal("expected a fold")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	msgs := obj["messages"].([]any)
	for _, raw := range msgs {
		m := raw.(map[string]any)
		if strings.EqualFold(strOr(m["role"]), "system") {
			t.Fatalf("a system message survived: %v", m)
		}
	}
	first := msgs[0].(map[string]any)
	if strOr(first["role"]) != "user" {
		t.Fatalf("first message role = %q, want user", strOr(first["role"]))
	}
	content := contentText(first["content"])
	if !strings.Contains(content, "SYS PROMPT") || !strings.Contains(content, "hello") {
		t.Fatalf("fold lost content: %q", content)
	}
	if len(msgs) != 2 {
		t.Fatalf("message count = %d, want 2 (system merged away)", len(msgs))
	}
}

// A conversation with no leading system message has nothing to fold, and the
// caller must fall through to the plain error rather than retrying blindly.
func TestFoldSystemIntoUser_NothingToFold(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	if _, ok := foldSystemIntoUser([]byte(in)); ok {
		t.Fatal("expected no fold when there is no system message")
	}
}

// Content-part arrays are the other shape clients send; the text must survive.
func TestFoldSystemIntoUser_PartsShape(t *testing.T) {
	in := `{"model":"m","messages":[
		{"role":"system","content":[{"type":"text","text":"PART SYS"}]},
		{"role":"user","content":[{"type":"text","text":"PART USER"}]}
	]}`
	out, ok := foldSystemIntoUser([]byte(in))
	if !ok {
		t.Fatal("expected a fold")
	}
	if !strings.Contains(string(out), "PART SYS") || !strings.Contains(string(out), "PART USER") {
		t.Fatalf("fold lost part content: %s", out)
	}
}

// An assistant-first conversation still needs its instructions delivered, so a
// user turn is synthesised ahead of it.
func TestFoldSystemIntoUser_NoUserTurn(t *testing.T) {
	in := `{"model":"m","messages":[
		{"role":"system","content":"SYS"},
		{"role":"assistant","content":"prior"}
	]}`
	out, ok := foldSystemIntoUser([]byte(in))
	if !ok {
		t.Fatal("expected a fold")
	}
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	first := msgs[0].(map[string]any)
	if strOr(first["role"]) != "user" {
		t.Fatalf("synthesised turn role = %q", strOr(first["role"]))
	}
	if !strings.Contains(contentText(first["content"]), "SYS") {
		t.Fatalf("synthesised turn lost the system text")
	}
}
