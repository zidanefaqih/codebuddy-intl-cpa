package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/zidanefaqih/codebuddy-intl-cpa/workbuddy/internal/trae/upstream"
)

func credential(uid, token string, expires int64) []byte {
	return JSON(map[string]any{"type": "trae", "label": "kept", "prefix": "team", "auth": map[string]any{"accessToken": token, "refreshToken": "refresh-old", "expiresAt": expires, "deviceId": "1234567890123456", "machineId": "machine", "domain": "trae.cn"}, "account": map[string]string{"uid": uid}})
}

func TestHostCancellationWhileRequestBlocked(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	host := func(method string, raw []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return OK(map[string]string{"operation_id": "pending"}), nil
		case pluginabi.MethodHostHTTPDo:
			close(started)
			<-canceled
			return nil, context.Canceled
		case pluginabi.MethodHostHTTPCancel:
			var req map[string]string
			_ = json.Unmarshal(raw, &req)
			if req["operation_id"] != "pending" || req["host_callback_id"] != "request-scope" {
				t.Error("wrong cancellation scope")
			}
			close(canceled)
			return OK(nil), nil
		}
		return nil, errors.New("unexpected callback")
	}
	e := New(host, "test")
	defer e.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.test", nil)
	done := make(chan error, 1)
	go func() { _, err := e.hostClient("", "request-scope").HTTP.Do(req); done <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation lost")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked callback did not cancel")
	}
}

func TestCandidatesRespectNamespaceAndModel(t *testing.T) {
	for _, policy := range []string{"allowed", "prefix", "disabled", "excluded", "priority", "model"} {
		t.Run(policy, func(t *testing.T) {
			first := credential("first", "one", time.Now().Add(4*time.Hour).Unix())
			second := credential("second", "two", time.Now().Add(4*time.Hour).Unix())
			var one, two map[string]any
			_ = json.Unmarshal(first, &one)
			_ = json.Unmarshal(second, &two)
			delete(one, "prefix")
			delete(two, "prefix")
			switch policy {
			case "prefix":
				two["prefix"] = "other"
			case "disabled":
				two["disabled"] = true
			case "excluded":
				two["excluded-models"] = []string{"chosen"}
			case "priority":
				two["priority"] = 2
			}
			first = JSON(one)
			second = JSON(two)
			host := func(method string, raw []byte) ([]byte, error) {
				switch method {
				case pluginabi.MethodHostAuthList:
					return OK(map[string]any{"files": []pluginapi.HostAuthFileEntry{{Name: "trae-second.json", AuthIndex: "second-index", Provider: Provider}}}), nil
				case pluginabi.MethodHostAuthGet:
					return OK(pluginapi.HostAuthGetResponse{Name: "trae-second.json", JSON: second}), nil
				}
				return nil, errors.New("unexpected callback")
			}
			e := New(host, "test")
			defer e.Stop()
			model := "chosen"
			if policy == "model" {
				model = "different"
			}
			e.modelCache["second"] = modelCache{at: time.Now(), models: []pluginapi.ModelInfo{{ID: model}}}
			got := e.candidates(executeRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "first-index", StorageJSON: first, Model: "chosen"}})
			want := 1
			if policy == "allowed" {
				want = 2
			}
			if len(got) != want {
				t.Fatalf("candidates=%d want=%d", len(got), want)
			}
		})
	}
}

func TestLoginExchangesAndPersistsViaHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == upstream.EpExchange {
			_, _ = io.WriteString(w, `{"Result":{"Token":"login-access","RefreshToken":"login-rotated","TokenExpireDuration":7200}}`)
		} else {
			_, _ = io.WriteString(w, `{"Result":{"UserID":"login-user","ScreenName":"name"}}`)
		}
	}))
	defer server.Close()
	var saved pluginapi.HostAuthSaveRequest
	e := testEngine(t, server, func(method string, raw []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthSave {
			return nil, errors.New("unexpected callback")
		}
		_ = json.Unmarshal(raw, &saved)
		return OK(nil), nil
	})
	e.config.CallbackPort = 0
	start, err := e.loginStart([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result pluginapi.AuthLoginStartResponse
	}
	_ = json.Unmarshal(start, &env)
	e.mu.Lock()
	trace := e.logins[env.Result.State].trace
	e.mu.Unlock()
	query := url.Values{"loginTraceID": {trace}, "refreshToken": {"login-old"}}
	if err = e.completeLogin(env.Result.State, "http://127.0.0.1:18080/authorize?"+query.Encode()); err != nil {
		t.Fatal(err)
	}
	if saved.Name != "trae-login-user.json" {
		t.Fatal("wrong auth file", saved.Name)
	}
	a, err := parseCredential(saved.JSON)
	if err != nil || a.JWT() != "login-access" || a.RefreshTokenValue() != "login-rotated" {
		t.Fatal("login credential not persisted", err)
	}
	if err = e.completeLogin(env.Result.State, "http://127.0.0.1:18080/authorize?"+query.Encode()); err == nil {
		t.Fatal("completed callback replayed")
	}
}
func testEngine(t *testing.T, server *httptest.Server, host HostCall) *Engine {
	t.Helper()
	e := New(host, "test")
	t.Cleanup(e.Stop)
	e.clientFactory = func(_, _ string) *upstream.Client {
		c := upstream.New()
		c.AgentHost = server.URL
		c.OAuthHost = server.URL
		c.UgHost = server.URL
		c.HTTP = server.Client()
		c.StreamHTTP = server.Client()
		return c
	}
	return e
}
func TestConcurrentRefreshPersistsOnce(t *testing.T) {
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Result":{"Token":"new-token","RefreshToken":"rotated","TokenExpireDuration":7200}}`)
	}))
	defer server.Close()
	initial := credential("u1", "old", time.Now().Add(-time.Minute).Unix())
	stored := initial
	var mu sync.Mutex
	saves := 0
	host := func(method string, raw []byte) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case pluginabi.MethodHostAuthGet:
			return OK(pluginapi.HostAuthGetResponse{Name: "custom.json", JSON: stored}), nil
		case pluginabi.MethodHostAuthSave:
			var req pluginapi.HostAuthSaveRequest
			_ = json.Unmarshal(raw, &req)
			if req.Name != "custom.json" {
				return nil, errors.New("filename changed")
			}
			stored = req.JSON
			saves++
			return OK(nil), nil
		}
		return nil, errors.New("unexpected callback")
	}
	e := testEngine(t, server, host)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, _, err := e.ensureAuth("index", initial, false, "", "")
			if err == nil && a.JWT() != "new-token" {
				err = errors.New("stale token")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 1 || saves != 1 {
		t.Fatalf("exchanges=%d saves=%d", count.Load(), saves)
	}
	var obj map[string]any
	_ = json.Unmarshal(stored, &obj)
	if obj["prefix"] != "team" || obj["label"] != "kept" {
		t.Fatal("lost CPA settings")
	}
}
func TestRunRetryOnlyBeforeOutput(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "early-failover", true: "partial-no-replay"}[partial], func(t *testing.T) {
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["model"] != "chosen" {
					t.Error("model changed")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if r.Header.Get("Authorization") == "Cloud-IDE-JWT first" {
					if partial {
						_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"partial\"}\n\n")
					}
					_, _ = io.WriteString(w, "event: error\ndata: {\"code\":1005,\"message\":\"limit\"}\n\n")
				} else {
					_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {}\n\n")
				}
			}))
			defer server.Close()
			e := testEngine(t, server, func(string, []byte) ([]byte, error) { return nil, errors.New("not stored") })
			until := time.Now().Add(4 * time.Hour).Unix()
			cands := []candidate{{id: "a", raw: credential("a", "first", until)}, {id: "b", raw: credential("b", "second", until)}}
			chunks := 0
			result, err := e.run(executeRequest{}, cands, []byte(`{"model":"chosen","messages":[]}`), "chosen", true, func([]byte) error { chunks++; return nil })
			if partial {
				if err == nil || count.Load() != 1 || chunks != 1 {
					t.Fatalf("partial stream replayed: count=%d chunks=%d err=%v", count.Load(), chunks, err)
				}
			} else {
				if err != nil || count.Load() != 2 || result.Content != "ok" {
					t.Fatalf("early retry failed: count=%d result=%+v err=%v", count.Load(), result, err)
				}
			}
		})
	}
}
func TestHostTransportPreservesFinalPayloadAndCancels(t *testing.T) {
	closed := 0
	host := func(method string, raw []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return OK(map[string]string{"operation_id": "op"}), nil
		case pluginabi.MethodHostHTTPDoStream:
			return OK(map[string]any{"status_code": 200, "stream_id": "s"}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			return OK(map[string]any{"payload": []byte("last bytes"), "done": true}), nil
		case pluginabi.MethodHostHTTPStreamClose:
			closed++
			return OK(nil), nil
		}
		return nil, errors.New("unexpected callback")
	}
	e := New(host, "test")
	defer e.Stop()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://example.test", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := e.hostClient("", "").StreamHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil || string(raw) != "last bytes" {
		t.Fatal("lost final bytes", string(raw), err)
	}
	_ = resp.Body.Close()
	_ = resp.Body.Close()
	if closed != 1 {
		t.Fatal("stream close not idempotent")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ = http.NewRequestWithContext(ctx, "GET", "https://example.test", nil)
	if _, err = e.hostClient("", "").HTTP.Do(req); err == nil {
		t.Fatal("canceled request submitted")
	}
}
func TestValidationAndPanel(t *testing.T) {
	e := New(func(string, []byte) ([]byte, error) { return OK(map[string]any{"files": []any{}}), nil }, "test")
	defer e.Stop()
	for _, src := range []string{`{"accessToken":"x","uid":"u","domain":"trae.ai"}`, `{"accessToken":"x","uid":"../escape"}`, `{"accessToken":"x","uid":"u","proxy_url":"http://proxy"}`} {
		if _, err := parseCredential([]byte(src)); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
	if _, err := e.Call(pluginabi.MethodPluginRegister, JSON(map[string]any{"config_yaml": []byte("max_attempts: 0")})); err == nil {
		t.Fatal("invalid config accepted")
	}
	res, err := e.manage(JSON(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/trae/panel"}))
	if err != nil {
		t.Fatal(err)
	}
	var env struct{ Result pluginapi.ManagementResponse }
	_ = json.Unmarshal(res, &env)
	if env.Result.StatusCode != 200 || !strings.Contains(string(env.Result.Body), "Trae 账号管理") {
		t.Fatal("panel resource route failed")
	}
	result, err := e.pick(JSON(pluginapi.SchedulerPickRequest{Provider: Provider, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "one", Provider: Provider}}}))
	if err != nil {
		t.Fatal(err)
	}
	var pick struct {
		Result pluginapi.SchedulerPickResponse
	}
	_ = json.Unmarshal(result, &pick)
	if pick.Result.Handled {
		t.Fatal("overrode default CPA scheduler")
	}
	e.config.CallbackPort = 0
	start, err := e.loginStart([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var login struct {
		Result pluginapi.AuthLoginStartResponse
	}
	_ = json.Unmarshal(start, &login)
	if err = e.completeLogin(login.Result.State, "http://localhost:18080?loginTraceID=wrong&refreshToken=secret"); err == nil {
		t.Fatal("mismatched callback accepted")
	}
}

func TestExecutorAcceptsCPAFormatAliases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {}\n\n")
	}))
	defer server.Close()
	e := testEngine(t, server, func(string, []byte) ([]byte, error) { return nil, errors.New("not stored") })
	until := time.Now().Add(4 * time.Hour).Unix()
	req := executeRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "chosen", Format: "openai", SourceFormat: "openai", Payload: []byte(`{"model":"chosen","messages":[{"role":"user","content":"hi"}]}`), StorageJSON: credential("a", "token", until)}}
	for _, alias := range []string{"", "none", "openai", "chat-completions", "openai-chat-completions"} {
		req.Format = alias
		if _, err := e.execute(JSON(req), false); err != nil {
			t.Fatalf("format %q rejected: %v", alias, err)
		}
	}
	req.Format = "responses"
	if _, err := e.execute(JSON(req), false); err == nil {
		t.Fatal("unsupported format accepted")
	}
}
