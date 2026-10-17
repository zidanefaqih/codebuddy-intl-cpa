// models_reload_test.go covers PLAN-0907b: the post-save host model-registry
// reload trigger (T1), the per-realm/per-account prefix routing fixes (T2/T3)
// and the prefix-collision validation.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// stubReloadTarget points the reload trigger at a test server and controls the
// mm_live_reload flag. Restores both package vars on cleanup.
func stubReloadTarget(t *testing.T, baseURL string, enabled bool) {
	t.Helper()
	selfURLMu.Lock()
	prevURL := selfURL
	selfURL = baseURL
	selfURLMu.Unlock()
	liveReloadMu.Lock()
	prevOn := liveReload
	liveReload = enabled
	liveReloadMu.Unlock()
	t.Cleanup(func() {
		selfURLMu.Lock()
		selfURL = prevURL
		selfURLMu.Unlock()
		liveReloadMu.Lock()
		liveReload = prevOn
		liveReloadMu.Unlock()
	})
}

type reloadCall struct {
	method string
	path   string
	authz  string
	mgmt   string
	rev    string
}

// reloadRecorder serves the plugin-config PATCH and records every call.
type reloadRecorder struct {
	mu     sync.Mutex
	calls  []reloadCall
	status int
}

func (rr *reloadRecorder) start(t *testing.T) *httptest.Server {
	t.Helper()
	if rr.status == 0 {
		rr.status = http.StatusOK
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			ModelsRev string `json:"models_rev"`
		}
		_ = json.Unmarshal(body, &payload)
		rr.mu.Lock()
		rr.calls = append(rr.calls, reloadCall{
			method: r.Method,
			path:   r.URL.Path,
			authz:  r.Header.Get("Authorization"),
			mgmt:   r.Header.Get("X-Management-Key"),
			rev:    payload.ModelsRev,
		})
		rr.mu.Unlock()
		w.WriteHeader(rr.status)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (rr *reloadRecorder) snapshot() []reloadCall {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return append([]reloadCall(nil), rr.calls...)
}

// TestMMTriggerHostReloadOK covers the happy path: PATCH method + path, only
// the management headers forwarded, and a models_rev that changes every call
// (the host skips a reload on an identical value).
func TestMMTriggerHostReloadOK(t *testing.T) {
	rr := &reloadRecorder{}
	srv := rr.start(t)
	stubReloadTarget(t, srv.URL, true)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer mgmt-secret")
	hdr.Set("X-Management-Key", "mk-1")

	first := mmTriggerHostReload(hdr)
	if first.Status != "ok" || first.HTTP != http.StatusOK {
		t.Fatalf("first reload = %+v, want ok/200", first)
	}
	second := mmTriggerHostReload(hdr)
	if second.Status != "ok" {
		t.Fatalf("second reload = %+v, want ok", second)
	}

	calls := rr.snapshot()
	if len(calls) != 2 {
		t.Fatalf("host calls = %d, want 2", len(calls))
	}
	wantPath := "/v0/management/plugins/" + pluginRouteID + "/config"
	for i, c := range calls {
		if c.method != http.MethodPatch {
			t.Errorf("call %d method = %q, want PATCH", i, c.method)
		}
		if c.path != wantPath {
			t.Errorf("call %d path = %q, want %q", i, c.path, wantPath)
		}
		if c.authz != "Bearer mgmt-secret" {
			t.Errorf("call %d Authorization = %q, want forwarded", i, c.authz)
		}
		if c.mgmt != "mk-1" {
			t.Errorf("call %d X-Management-Key = %q, want forwarded", i, c.mgmt)
		}
		if c.rev == "" {
			t.Errorf("call %d has empty models_rev", i)
		}
	}
	if calls[0].rev == calls[1].rev {
		t.Fatalf("models_rev must differ per call, both %q", calls[0].rev)
	}
}

func TestMMTriggerHostReloadNoCredentials(t *testing.T) {
	rr := &reloadRecorder{}
	srv := rr.start(t)
	stubReloadTarget(t, srv.URL, true)

	if got := mmTriggerHostReload(http.Header{}); got.Status != "no_credentials" {
		t.Fatalf("status = %q, want no_credentials", got.Status)
	}
	if calls := rr.snapshot(); len(calls) != 0 {
		t.Fatalf("host must not be called without credentials, got %d calls", len(calls))
	}
}

func TestMMTriggerHostReloadUnauthorized(t *testing.T) {
	rr := &reloadRecorder{status: http.StatusUnauthorized}
	srv := rr.start(t)
	stubReloadTarget(t, srv.URL, true)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer wrong")
	got := mmTriggerHostReload(hdr)
	if got.Status != "failed" || got.HTTP != http.StatusUnauthorized {
		t.Fatalf("reload = %+v, want failed/401", got)
	}
	if strings.Contains(got.Error, "wrong") {
		t.Fatalf("error must not echo the credential: %q", got.Error)
	}
}

func TestMMTriggerHostReloadDisabled(t *testing.T) {
	rr := &reloadRecorder{}
	srv := rr.start(t)
	stubReloadTarget(t, srv.URL, false)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer x")
	if got := mmTriggerHostReload(hdr); got.Status != "disabled" {
		t.Fatalf("status = %q, want disabled", got.Status)
	}
	if calls := rr.snapshot(); len(calls) != 0 {
		t.Fatalf("disabled reload must not call the host, got %d calls", len(calls))
	}
}

// TestModelsConfigSaveTriggersReload verifies the Save response carries the
// reload outcome and that the trigger actually fired once.
func TestModelsConfigSaveTriggersReload(t *testing.T) {
	useTempStateDir(t)
	origAccounts := mmAccountsForRealmFn
	mmAccountsForRealmFn = func(realm string) []string { return nil }
	t.Cleanup(func() { mmAccountsForRealmFn = origAccounts })

	rr := &reloadRecorder{}
	srv := rr.start(t)
	stubReloadTarget(t, srv.URL, true)

	out := handleModelsConfigPost(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/workbuddy/models/config",
		Query:  map[string][]string{},
		Headers: http.Header{
			"Authorization": []string{"Bearer mgmt-secret"},
		},
		Body: []byte(`{"realm":"global","scope":"all","state":{"prefix_enabled":true,"prefix_value":"yes"}}`),
	})
	if out["error"] != nil {
		t.Fatalf("save failed: %v", out["error"])
	}
	reload, ok := out["reload"].(mmReloadResult)
	if !ok {
		t.Fatalf("reload field missing/wrong type: %#v", out["reload"])
	}
	if reload.Status != "ok" {
		t.Fatalf("reload = %+v, want ok", reload)
	}
	if calls := rr.snapshot(); len(calls) != 1 {
		t.Fatalf("host calls = %d, want 1", len(calls))
	}
}

// TestPrefixRoutingCustomRealm covers T2: a custom Global prefix strips on a
// Global account and a custom CN prefix is recognised as CN.
func TestPrefixRoutingCustomRealm(t *testing.T) {
	useTempStateDir(t)
	if err := updateRealmSettings(realmGlobal, mmSettings{Prefix: mmPrefixState{On: true, Val: "yes"}}); err != nil {
		t.Fatalf("seed global prefix: %v", err)
	}
	if err := updateRealmSettings(realmCN, mmSettings{Prefix: mmPrefixState{On: true, Val: "zzz"}}); err != nil {
		t.Fatalf("seed cn prefix: %v", err)
	}

	if got, ok := stripDynamicPrefix("yes/deepseek-v4.1-flash", realmGlobal); !ok || got != "deepseek-v4.1-flash" {
		t.Fatalf("global custom strip = %q/%v", got, ok)
	}
	if got := resolveUpstreamModel("yes/deepseek-v4.1-flash", realmGlobal, nil); got != "deepseek-v4.1-flash" {
		t.Fatalf("resolveUpstreamModel global custom = %q", got)
	}
	if isCNModelID("yes/deepseek-v4.1-flash") {
		t.Fatal("global custom prefix must not classify as CN")
	}
	if !isCNModelID("zzz/glm-5.3") {
		t.Fatal("custom CN prefix must classify as CN")
	}
	if !isCNModelID("cn/glm-5.3") {
		t.Fatal("legacy cn/ must stay CN")
	}
	if isCNModelID("glm-5.3") {
		t.Fatal("bare id must not classify as CN")
	}
	if got := mmPrefixRealm("zzz/glm-5.3"); got != realmCN {
		t.Fatalf("mmPrefixRealm(custom CN) = %q, want cn", got)
	}
	if got := mmPrefixRealm("yes/deepseek-v4.1-flash"); got != realmGlobal {
		t.Fatalf("mmPrefixRealm(custom Global) = %q, want global", got)
	}
	if got := mmPrefixRealm("glm-5.3"); got != "" {
		t.Fatalf("mmPrefixRealm(bare) = %q, want empty", got)
	}
}

// TestValidatePrefixSettings covers the collision + character rules.
func TestValidatePrefixSettings(t *testing.T) {
	useTempStateDir(t)
	// Seed a custom CN prefix so the global/CN collision check is exercised.
	if err := updateRealmSettings(realmCN, mmSettings{Prefix: mmPrefixState{On: true, Val: "shared"}}); err != nil {
		t.Fatalf("seed cn prefix: %v", err)
	}

	cases := []struct {
		name    string
		realm   string
		s       mmSettings
		wantErr bool
	}{
		{"global custom ok", realmGlobal, mmSettings{Prefix: mmPrefixState{On: true, Val: "yes"}}, false},
		{"global trailing slash ok", realmGlobal, mmSettings{Prefix: mmPrefixState{On: true, Val: "yes/"}}, false},
		{"global empty means default", realmGlobal, mmSettings{Prefix: mmPrefixState{On: true, Val: ""}}, false},
		{"cn empty means default", realmCN, mmSettings{Prefix: mmPrefixState{On: true, Val: ""}}, false},
		{"global reserved cn", realmGlobal, mmSettings{Prefix: mmPrefixState{On: true, Val: "cn"}}, true},
		{"cn reserved cb", realmCN, mmSettings{Prefix: mmPrefixState{On: true, Val: "cb"}}, true},
		{"global collides with cn", realmGlobal, mmSettings{Prefix: mmPrefixState{On: true, Val: "shared"}}, true},
		{"bad characters", realmGlobal, mmSettings{Prefix: mmPrefixState{On: true, Val: "bad prefix!"}}, true},
		{"off ignores value", realmGlobal, mmSettings{Prefix: mmPrefixState{On: false, Val: "cn/"}}, false},
	}
	for _, c := range cases {
		err := validatePrefixSettings(c.realm, c.s)
		if c.wantErr && err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
		}
	}
}

// TestBuildRequestFailoverPoolRealmPrefix covers T2's pool symmetry: a custom
// Global prefix gets a Global-only pool, a custom CN prefix a CN-only pool.
func TestBuildRequestFailoverPoolRealmPrefix(t *testing.T) {
	useTempStateDir(t)
	if err := updateRealmSettings(realmGlobal, mmSettings{Prefix: mmPrefixState{On: true, Val: "yes"}}); err != nil {
		t.Fatalf("seed global prefix: %v", err)
	}
	if err := updateRealmSettings(realmCN, mmSettings{Prefix: mmPrefixState{On: true, Val: "zzz"}}); err != nil {
		t.Fatalf("seed cn prefix: %v", err)
	}
	files := []pluginapi.HostAuthFileEntry{
		testAuthEntry("g-idx", "guid", false),
		testAuthEntry("c-idx", "cuid", false),
	}
	creds := map[string]*storedAuth{
		"g-idx": {Auth: storedTokens{Domain: "https://www.codebuddy.ai"}, Account: storedAccount{UID: "guid"}},
		"c-idx": {Auth: storedTokens{Domain: "https://www.codebuddy.cn"}, Account: storedAccount{UID: "cuid"}},
	}
	stubAuthSources(t, files, creds)

	pool, storage, err := buildRequestFailoverPool("yes/deepseek-v4.1-flash", "guid", nil)
	if err != nil {
		t.Fatalf("global pool: %v", err)
	}
	if pool.Primary == nil || pool.Primary.AuthID != "g-idx" {
		t.Fatalf("global primary = %+v, want g-idx", pool.Primary)
	}
	if len(pool.Others) != 0 {
		t.Fatalf("global pool leaked CN peers: %+v", pool.Others)
	}
	if len(storage) == 0 {
		t.Fatal("global pool must return the selected account storage")
	}

	pool, _, err = buildRequestFailoverPool("zzz/glm-5.3", "cuid", nil)
	if err != nil {
		t.Fatalf("cn pool: %v", err)
	}
	if pool.Primary == nil || pool.Primary.AuthID != "c-idx" {
		t.Fatalf("cn primary = %+v, want c-idx", pool.Primary)
	}
	if len(pool.Others) != 0 {
		t.Fatalf("cn pool leaked Global peers: %+v", pool.Others)
	}
}
