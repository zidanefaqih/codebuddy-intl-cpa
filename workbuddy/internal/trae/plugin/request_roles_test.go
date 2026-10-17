package plugin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/zidanefaqih/codebuddy-intl-cpa/workbuddy/internal/trae/upstream"
)

func TestRestoreDeveloperRoles(t *testing.T) {
	for _, tc := range []struct {
		name, original, translated, roles string
	}{
		{"codex", `{"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},{"role":"user","content":"hello"}]}`, `{"messages":[{"role":"user","content":[{"type":"text","text":"rules"}]},{"role":"user","content":"hello"}]}`, "system,user"},
		{"duplicate user text", `{"instructions":"rules","input":[{"role":"user","content":"rules"},{"role":"developer","content":"rules"},{"role":"user","content":"rules"}]}`, `{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"rules"},{"role":"user","content":"rules"},{"role":"user","content":"rules"}]}`, "system,user,system,user"},
		{"tool history", `{"input":[{"role":"developer","content":"rules"},{"type":"function_call_output","output":"rules"},{"role":"user","content":"continue"}]}`, `{"messages":[{"role":"user","content":"rules"},{"role":"assistant","tool_calls":[{"id":"a"}]},{"role":"tool","tool_call_id":"a","content":"rules"},{"role":"user","content":"continue"}]}`, "system,assistant,tool,user"},
		{"harness", `{"input":[{"role":"system","content":"rules"},{"role":"user","content":"hello"}]}`, `{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello"}]}`, "system,user"},
		{"unmatched", `{"input":[{"role":"developer","content":"rules"}]}`, `{"messages":[{"role":"user","content":"different"}]}`, "user"},
		{"mixed content", `{"input":[{"role":"developer","content":[{"type":"input_text","text":"rules"},{"type":"input_image","image_url":"image"}]}]}`, `{"messages":[{"role":"user","content":"rules"}]}`, "user"},
		{"no original", `{}`, `{"messages":[{"role":"user","content":"rules"}]}`, "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(tc.translated), &body); err != nil {
				t.Fatal(err)
			}
			restoreDeveloperRoles(body, []byte(tc.original))
			var roles []string
			for _, m := range body["messages"].([]any) {
				roles = append(roles, m.(map[string]any)["role"].(string))
			}
			if strings.Join(roles, ",") != tc.roles {
				t.Fatalf("roles=%v want %s", roles, tc.roles)
			}
		})
	}
}

func TestExecuteCodexResponsesToolRoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		messages := body["messages"].([]any)
		if messages[0].(map[string]any)["role"] != "system" {
			t.Error("developer instruction lost")
		}
		if body["model"] != "Doubao-Seed-2.0-Code" || body["response_format"] == nil {
			t.Error("model or structured output lost")
		}
		if len(body["tools"].([]any)) != 1 {
			t.Error("tool definitions lost")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event:progress_notice\ndata:\"Processing_1791709582\"\n\n")
		if len(messages) == 2 {
			_, _ = io.WriteString(w, "event:output\ndata:{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function_call\":{\"name\":\"read_file\",\"arguments\":\"{}\"}}]}\n\nevent:done\ndata:{}\n\n")
		} else {
			call := messages[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
			if call["function_call"] == nil || messages[3].(map[string]any)["tool_call_id"] != "call_1" {
				t.Error("tool result association lost")
			}
			_, _ = io.WriteString(w, "event:output\ndata:{\"response\":\"finished\"}\n\nevent:done\ndata:{}\n\n")
		}
	}))
	defer server.Close()
	e := testEngine(t, server, func(string, []byte) ([]byte, error) { return OK(map[string]any{"files": []any{}}), nil })
	for _, stream := range []bool{false, true} {
		for _, followup := range []bool{false, true} {
			body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "rules"}, map[string]any{"role": "user", "content": "read"}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "parameters": map[string]any{"type": "object"}}}}, "response_format": map[string]any{"type": "json_schema"}}
			if followup {
				body["messages"] = append(body["messages"].([]any), map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}}}}, map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "file contents"})
			}
			req := executeRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthProvider: Provider, Model: "Doubao-Seed-2.0-Code", Format: "openai", SourceFormat: "openai-response", OriginalRequest: []byte(`{"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},{"role":"user","content":"read"}]}`), Payload: JSON(body), StorageJSON: credential("fixture", "access", time.Now().Add(4*time.Hour).Unix()), Metadata: map[string]any{"request_path": "/v1/responses"}}}
			out, err := e.execute(JSON(req), stream)
			if err != nil {
				t.Fatal(err)
			}
			var env struct {
				Result struct {
					Payload []byte
					Chunks  []pluginapi.ExecutorStreamChunk
				}
			}
			if err := json.Unmarshal(out, &env); err != nil {
				t.Fatal(err)
			}
			if stream {
				if len(env.Result.Chunks) != 2 || !strings.HasPrefix(string(env.Result.Chunks[0].Payload), "data: ") {
					t.Fatal("Responses translator stream framing lost")
				}
				if !strings.Contains(string(env.Result.Chunks[1].Payload), map[bool]string{false: `"finish_reason":"tool_calls"`, true: `"finish_reason":"stop"`}[followup]) {
					t.Fatal("incorrect terminal chunk")
				}
			} else {
				var c struct {
					Choices []struct {
						FinishReason string `json:"finish_reason"`
						Message      struct {
							Content   string
							ToolCalls []upstream.Call `json:"tool_calls"`
						}
					}
				}
				if err := json.Unmarshal(env.Result.Payload, &c); err != nil {
					t.Fatal(err)
				}
				if len(c.Choices) != 1 {
					t.Fatal("missing completion")
				}
				if followup && c.Choices[0].Message.Content != "finished" || !followup && (c.Choices[0].FinishReason != "tool_calls" || len(c.Choices[0].Message.ToolCalls) != 1) {
					t.Fatal("tool round trip failed")
				}
			}
		}
	}
}
