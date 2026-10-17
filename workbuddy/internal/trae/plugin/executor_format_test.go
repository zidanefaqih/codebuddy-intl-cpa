package plugin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestExecuteCPANormalizedFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req["model"] != "selected" || len(req["messages"].([]any)) != 2 {
			t.Error("lost translated request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"answer\"}\n\nevent: done\ndata: {}\n\n")
	}))
	defer server.Close()
	e := testEngine(t, server, func(string, []byte) ([]byte, error) { return OK(map[string]any{"files": []any{}}), nil })
	for _, format := range []string{"openai", "chat-completions", "openai_chat_completions"} {
		for _, stream := range []bool{false, true} {
			t.Run(format+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				req := executeRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthProvider: Provider, Model: "selected", Format: format, SourceFormat: "openai", StorageJSON: credential("fixture", "access", time.Now().Add(4*time.Hour).Unix()), Payload: []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"question"}]}`), Metadata: map[string]any{"request_path": "/v1/chat/completions"}}}
				out, err := e.execute(JSON(req), stream)
				if err != nil {
					t.Fatal(err)
				}
				if stream {
					var env struct {
						Result struct {
							Chunks []pluginapi.ExecutorStreamChunk
						}
					}
					_ = json.Unmarshal(out, &env)
					if len(env.Result.Chunks) != 2 {
						t.Fatal("missing stream chunks")
					}
				} else {
					var env struct{ Result pluginapi.ExecutorResponse }
					_ = json.Unmarshal(out, &env)
					var completion struct {
						Model   string
						Choices []struct{ Message struct{ Content string } }
					}
					_ = json.Unmarshal(env.Result.Payload, &completion)
					if completion.Model != "selected" || len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "answer" {
						t.Fatal("invalid completion")
					}
				}
			})
		}
	}
	for _, pair := range [][2]string{{"openai-response", "openai"}, {"openai", "claude"}} {
		_, err := e.execute(JSON(executeRequest{ExecutorRequest: pluginapi.ExecutorRequest{Format: pair[0], SourceFormat: pair[1]}}), false)
		if err == nil {
			t.Fatal("accepted untranslated protocol")
		}
	}
}
