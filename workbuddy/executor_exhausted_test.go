package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type exhaustedMockTransport func(*http.Request) (*http.Response, error)

func (f exhaustedMockTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Run the actual executor entry points. This transport rejects all external
// traffic and supplies only fake chat responses, so no paid request can escape.
func TestExecutorCreditFailoverIntegration(t *testing.T) {
	enableCacheTrace(t) // the assertions below read the per-attempt trace
	for _, mode := range []string{"nonstream", "syncstream", "asyncstream"} {
		for _, all := range []bool{false, true} {
			name := mode + "/success"
			if all {
				name = mode + "/all-exhausted"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
				oldEmit := streamEmitSource
				streamEmitSource = func(string, []byte) error { return nil }
				t.Cleanup(func() { streamEmitSource = oldEmit })
				cands := exhaustedCandidates(t)
				for _, c := range cands {
					c.Auth.Auth.AccessToken = "fake-test-token"
				}
				files := []pluginapi.HostAuthFileEntry{testAuthEntry("exhaust-a", "record-a", false), testAuthEntry("exhaust-b", "record-b", false)}
				stubAuthSources(t, files, map[string]*storedAuth{"exhaust-a": cands[0].Auth, "exhaust-b": cands[1].Auth})
				oldStore := usageStore
				usageStore = &requestUsageStore{perDay: map[string]map[string]*dayAgg{}}
				t.Cleanup(func() { usageStore = oldStore })
				// Seed the same resolver used by production record hooks.
				usageMetaCache.mu.Lock()
				oldID, oldUID, oldIndex, oldAt := usageMetaCache.byAuthID, usageMetaCache.byUID, usageMetaCache.byIndex, usageMetaCache.refreshed
				usageMetaCache.byAuthID = map[string]usageAuthMeta{}
				usageMetaCache.byUID = map[string]usageAuthMeta{"uid-a": {AuthIndex: "exhaust-a"}, "uid-b": {AuthIndex: "exhaust-b"}}
				usageMetaCache.byIndex = map[string]usageAuthMeta{}
				usageMetaCache.refreshed = time.Now()
				usageMetaCache.mu.Unlock()
				t.Cleanup(func() {
					usageMetaCache.mu.Lock()
					usageMetaCache.byAuthID, usageMetaCache.byUID, usageMetaCache.byIndex, usageMetaCache.refreshed = oldID, oldUID, oldIndex, oldAt
					usageMetaCache.mu.Unlock()
				})
				client := sharedHTTPClient()
				oldTransport := client.Transport
				var order, cids, bodies []string
				client.Transport = exhaustedMockTransport(func(r *http.Request) (*http.Response, error) {
					if !strings.HasSuffix(r.URL.Path, "/v2/chat/completions") {
						t.Fatalf("unexpected HTTP call: %s", r.URL.Path)
					}
					body, _ := io.ReadAll(r.Body)
					order = append(order, r.Header.Get("X-User-Id"))
					cids = append(cids, r.Header.Get("X-Conversation-ID"))
					bodies = append(bodies, string(body))
					status, payload := 429, `{"error":{"data":{"code":14018,"msg":"Credits exhausted"}}}`
					if r.Header.Get("X-User-Id") == "uid-b" && !all {
						status = 200
						payload = "data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":1,\"credit\":0.01}}\n\ndata: [DONE]\n\n"
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload))}, nil
				})
				t.Cleanup(func() { client.Transport = oldTransport })
				req := executorStreamRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "cb/gpt-6.1-sol", AuthID: "exhaust-a", StorageJSON: storageJSONForAuth(cands[0].Auth), Payload: []byte(`{"model":"cb/gpt-6.1-sol","messages":[{"role":"system","content":"test"},{"role":"user","content":"hello"}]}`)}}
				var err error
				raw, _ := json.Marshal(req)
				switch mode {
				case "nonstream":
					_, err = handleExecExecute(raw)
				case "syncstream":
					_, err = handleExecStream(raw)
				default:
					// Call the real async pump synchronously so test cleanup cannot race it.
					pool, storage, e := buildRequestFailoverPool(req.Model, req.AuthID, req.StorageJSON)
					if e != nil {
						t.Fatal(e)
					}
					body := prepareUpstreamBody(req.Payload, nil, cands[0].Auth, "gpt-6.1-sol")
					pumpWithFailover(t.Context(), nil, req, body, "gpt-6.1-sol", "uid-a", time.Now(), false, pool.ordered(storage))
				}
				if all && mode != "asyncstream" && err == nil {
					t.Fatal("expected terminal credit rejection")
				}
				if !all && err != nil {
					t.Fatal(err)
				}
				if len(order) != 2 || order[0] != "uid-a" || order[1] != "uid-b" {
					t.Fatalf("order=%v", order)
				}
				if cids[0] == "" || cids[0] != cids[1] || bodies[0] != bodies[1] {
					t.Fatal("failover changed body/CID")
				}
				records := usageStore.ringSnapshot()
				if len(records) != 1 || records[0].AuthID != "exhaust-b" || records[0].AuthIndex != "exhaust-b" || records[0].Failed != all {
					t.Fatalf("records=%+v", records)
				}
				trace := records[0].CacheTrace
				if trace == nil || len(trace.Attempts) != 2 || trace.Attempts[0].Status != 429 {
					t.Fatalf("missing per-attempt trace: %+v", trace)
				}
				wantFinalStatus := 200
				if all {
					wantFinalStatus = 429
				}
				if trace.Attempts[1].Status != wantFinalStatus {
					t.Fatalf("final HTTP status lost: %+v", trace.Attempts)
				}
				if trace.Attempts[0].CID != cids[0] || trace.Attempts[1].CID != cids[1] {
					t.Fatal("trace did not capture actual upstream CIDs")
				}
				if trace.Upstream.BodyHash != traceHash([]byte(bodies[0])) || trace.RequestID == "" {
					t.Fatal("trace does not match outbound body")
				}
				if !candidateOnCooldown(cands[0]) {
					t.Fatal("exhausted account was not skipped")
				}
			})
		}
	}
}
