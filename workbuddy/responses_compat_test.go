package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestSSEAuxiliaryEventsAreNotCompletionChunks(t *testing.T) {
	sse := ":keepalive\n\nevent:progress_notice\ndata:\"Processing_123\"\n\n" +
		"event:metadata\ndata:{\"session_id\":\"fixture\"}\n\n" +
		"event:message\ndata:{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
		"data:{\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":6}}\n\ndata:[DONE]\n\n"
	collector := &sseUsageCollector{}
	chunks, err := aggregateSSEWithCollector(strings.NewReader(sse), true, collector)
	if err != nil || len(chunks) != 2 {
		t.Fatalf("chunks=%d error=%v; auxiliary events must not be forwarded", len(chunks), err)
	}
	for _, chunk := range chunks {
		var obj map[string]any
		if json.Unmarshal([]byte(stripDataPrefix(string(chunk.Payload))), &obj) != nil || obj["choices"] == nil {
			t.Fatalf("forwarded non-completion event: %s", chunk.Payload)
		}
	}
	if collector.lastMap()["total_tokens"] != float64(6) {
		t.Fatal("usage lost")
	}
	completion, saw, err := aggregateCompletionCounted(strings.NewReader(sse), "fixture")
	if err != nil || !saw || !strings.Contains(string(completion), `"content":"hello"`) {
		t.Fatalf("aggregation=%s saw=%v error=%v", completion, saw, err)
	}
}

func TestProgressOnlyStreamsFailAllExecutorPaths(t *testing.T) {
	client := sharedHTTPClient()
	original := client.Transport
	defer func() { client.Transport = original }()
	client.Transport = executorErrorTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event:progress_notice\ndata:\"Processing_123\"\n\n")), Request: req}, nil
	})
	cand := &failoverCandidate{AuthID: "fixture", Auth: &storedAuth{}}
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"non-stream", func() error {
			_, err := upstreamAttempt(context.Background(), []byte(`{}`), "fixture", cand)
			return err
		}},
		{"collected-stream", func() error {
			_, _, err := collectStreamAttempt([]byte(`{}`), cand, true, &sseUsageCollector{})
			return err
		}},
		{"async-stream", func() error {
			req, _ := http.NewRequest("POST", endpointChatFor(cand.Auth), strings.NewReader(`{}`))
			_, _, err := pumpAttempt(req, "fixture", true, "fixture", "fixture", "fixture", "fixture", time.Now(), &sseUsageCollector{})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.Is(err, errEmptyStream) {
				t.Fatalf("got %v, want empty stream", err)
			}
		})
	}
}

func TestPrepareRestoresCPADowngradedDeveloperRole(t *testing.T) {
	original := []byte(`{"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},{"role":"user","content":"rules"}]}`)
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"rules"}]},{"role":"user","content":"rules"}],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}],"response_format":{"type":"json_schema"}}`)
	var body map[string]any
	if err := json.Unmarshal(prepareUpstreamBody(payload, original, &storedAuth{}, "exact-model"), &body); err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["role"] != "user" {
		t.Fatalf("developer or user roles changed incorrectly: %v", msgs)
	}
	if body["model"] != "exact-model" || body["tools"] == nil || body["response_format"] == nil {
		t.Fatal("translated request fields lost")
	}
}

func TestSSEFrameBoundariesAndErrors(t *testing.T) {
	// SSE can split one JSON object over multiple data fields, with CRLF and
	// id/retry fields. Only the joined payload should reach the translator.
	sse := "id:1\r\nretry:1000\r\nevent:message\r\ndata:{\r\ndata:\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\r\n\r\ndata:[DONE]\r\n\r\ndata:garbage\r\n\r\n"
	chunks, err := aggregateSSEWithCollector(strings.NewReader(sse), false, nil)
	if err != nil || len(chunks) != 1 || !json.Valid(chunks[0].Payload) {
		t.Fatalf("multiline SSE failed: chunks=%v error=%v", chunks, err)
	}
	framed, err := aggregateSSEWithCollector(strings.NewReader(sse), true, nil)
	if err != nil || len(framed) != 1 || strings.Contains(string(framed[0].Payload), "\n") || !json.Valid([]byte(stripDataPrefix(string(framed[0].Payload)))) {
		t.Fatalf("multiline SSE cannot be consumed as one framed chunk: %v, %v", framed, err)
	}
	for _, sse := range []string{
		"event:error\ndata:{\"message\":\"rejected\"}\n\n",
		"data:{\"error\":{\"message\":\"rejected\"}}\n\n",
		"data:not-json\n\n",
	} {
		if _, err := aggregateSSEWithCollector(strings.NewReader(sse), false, nil); err == nil {
			t.Fatalf("swallowed error SSE: %s", sse)
		}
		if _, _, err := aggregateCompletionCounted(strings.NewReader(sse), "fixture"); err == nil {
			t.Fatalf("swallowed aggregation error: %s", sse)
		}
	}
	_, saw, err := aggregateCompletionCounted(strings.NewReader("data:{\"usage\":{\"total_tokens\":1}}\n\n"), "fixture")
	if err != nil || saw {
		t.Fatalf("usage-only stream was counted as model output: saw=%v error=%v", saw, err)
	}
}

func TestSSEMultilineEventLimit(t *testing.T) {
	line := "data:" + strings.Repeat("x", 2*1024*1024) + "\n"
	if err := scanCompletionSSE(strings.NewReader(line+line+"\n"), func(string) error {
		t.Fatal("oversized event reached completion consumer")
		return nil
	}); err == nil || !strings.Contains(err.Error(), "event exceeds") {
		t.Fatalf("multiline event was not bounded: %v", err)
	}
}

// Exercise the real entry points so streaming cannot silently drop
// OriginalRequest before the role-restoration helper sees it.
func TestExecutorRestoresOriginalDeveloperRoles(t *testing.T) {
	for _, mode := range []string{"nonstream", "stream"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
			cand := exhaustedCandidates(t)[0]
			cand.Auth.Auth.AccessToken = "fixture-token"
			stubAuthSources(t, []pluginapi.HostAuthFileEntry{testAuthEntry(cand.AuthID, cand.RecordID, false)}, map[string]*storedAuth{cand.AuthID: cand.Auth})
			oldStore := usageStore
			usageStore = &requestUsageStore{perDay: map[string]map[string]*dayAgg{}}
			t.Cleanup(func() { usageStore = oldStore })
			client := sharedHTTPClient()
			oldTransport := client.Transport
			t.Cleanup(func() { client.Transport = oldTransport })
			calls := 0
			client.Transport = executorErrorTransport(func(r *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(r.URL.Path, "/v2/chat/completions") {
					t.Fatalf("unexpected HTTP call: %s", r.URL.Path)
				}
				calls++
				raw, _ := io.ReadAll(r.Body)
				var body map[string]any
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Fatal(err)
				}
				messages, _ := body["messages"].([]any)
				if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["role"] != "user" {
					t.Fatalf("lost developer role on %s: %s", mode, raw)
				}
				payload := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})
			req := executorStreamRequest{ExecutorRequest: pluginapi.ExecutorRequest{
				Model: "gpt-6.1-sol", AuthID: cand.AuthID, StorageJSON: storageJSONForAuth(cand.Auth),
				Payload:         []byte(`{"messages":[{"role":"user","content":"rules"},{"role":"user","content":"hi"}]}`),
				OriginalRequest: []byte(`{"input":[{"role":"developer","content":"rules"},{"role":"user","content":"hi"}]}`),
			}}
			raw, _ := json.Marshal(req)
			var err error
			if mode == "stream" {
				_, err = handleExecStream(raw)
			} else {
				_, err = handleExecExecute(raw)
			}
			if err != nil || calls != 1 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestRestoreRolesKeepsHarnessAndToolHistory(t *testing.T) {
	for _, tc := range []struct{ name, original, translated, roles string }{
		{"duplicate text", `{"instructions":"rules","input":[{"role":"user","content":"rules"},{"role":"developer","content":"rules"},{"role":"user","content":"rules"}]}`, `{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"rules"},{"role":"user","content":"rules"},{"role":"user","content":"rules"}]}`, "system,user,system,user"},
		{"harness", `{"input":[{"role":"system","content":"rules"},{"role":"user","content":"hi"}]}`, `{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hi"}]}`, "system,user"},
		{"tool history", `{"input":[{"role":"developer","content":"rules"},{"type":"function_call_output","output":"rules"},{"role":"user","content":"continue"}]}`, `{"messages":[{"role":"user","content":"rules"},{"role":"assistant","tool_calls":[{"id":"a"}]},{"role":"tool","tool_call_id":"a","content":"rules"},{"role":"user","content":"continue"}]}`, "system,assistant,tool,user"},
		{"mixed content", `{"input":[{"role":"developer","content":[{"type":"input_text","text":"rules"},{"type":"input_image","image_url":"image"}]}]}`, `{"messages":[{"role":"user","content":"rules"}]}`, "user"},
		{"no original", `{}`, `{"messages":[{"role":"user","content":"rules"}]}`, "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(tc.translated), &body); err != nil {
				t.Fatal(err)
			}
			restoreResponsesDeveloperRoles(body, []byte(tc.original))
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
