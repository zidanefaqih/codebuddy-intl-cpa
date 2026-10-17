// models_config_test.go covers the Model Manager backend (WORKER-1a):
// catalog/legacy parity, upstream discovery + union/dedup, the state file
// (atomic save, corrupt recovery) and the three-layer settings resolution plus
// the GET/POST /models/config contract.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// useTempStateDir points the plugin data dir at a temp dir and clears the
// in-memory caches so each test starts from a clean state file.
func useTempStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WORKBUDDY_DATA_DIR", dir)
	resetModelsConfigCaches()
	t.Cleanup(resetModelsConfigCaches)
	return dir
}

func resetModelsConfigCaches() {
	modelsStateMu.Lock()
	modelsStateCache = nil
	modelsStatePath = ""
	modelsStateMu.Unlock()
	mmFetchCache.mu.Lock()
	mmFetchCache.entries = map[string]mmFetchCacheEntry{}
	mmFetchCache.mu.Unlock()
}

// TestCatalogCoversLegacyEntries is the drift guard for the catalog move:
// every entry of the legacy wbModels()/wbCNModels() tables must be covered by
// the new catalog API with identical values, plus the metadata entries (tier
// aliases, verified hidden models).
func TestCatalogCoversLegacyEntries(t *testing.T) {
	cases := []struct {
		realm  string
		legacy []pluginapi.ModelInfo
	}{
		{realmGlobal, wbModels()},
		{realmCN, wbCNModels()},
	}
	for _, tc := range cases {
		defs := catalogModels(tc.realm)
		byID := map[string]modelDef{}
		for _, d := range defs {
			if d.Realm != tc.realm {
				t.Errorf("%s: %q has realm %q", tc.realm, d.ID, d.Realm)
			}
			byID[d.ID] = d
		}
		if len(byID) != len(defs) {
			t.Errorf("%s: catalog has duplicate IDs (%d entries, %d unique)", tc.realm, len(defs), len(byID))
		}
		for _, legacy := range tc.legacy {
			def, ok := byID[legacy.ID]
			if !ok {
				t.Errorf("%s: legacy model %q missing from catalog", tc.realm, legacy.ID)
				continue
			}
			if def.Name != legacy.Name {
				t.Errorf("%s: %q name = %q, legacy %q", tc.realm, legacy.ID, def.Name, legacy.Name)
			}
			if def.Ctx != legacy.ContextLength {
				t.Errorf("%s: %q ctx = %d, legacy %d", tc.realm, legacy.ID, def.Ctx, legacy.ContextLength)
			}
			if def.MaxTok != legacy.MaxCompletionTokens {
				t.Errorf("%s: %q maxTok = %d, legacy %d", tc.realm, legacy.ID, def.MaxTok, legacy.MaxCompletionTokens)
			}
		}
		for _, meta := range wbCatalogMeta[tc.realm] {
			if _, ok := byID[meta.ID]; !ok {
				t.Errorf("%s: metadata model %q missing from catalog", tc.realm, meta.ID)
			}
		}
		for key := range hiddenLegacyIDs {
			realm, id, _ := strings.Cut(key, "/")
			if realm != tc.realm {
				continue
			}
			if def, ok := byID[id]; ok && !def.Hidden {
				t.Errorf("%s: legacy hidden model %q lost its Hidden flag", tc.realm, id)
			}
		}
	}
	// The host-facing conversion never exposes an entry without an ID or with
	// non-positive token limits.
	for _, realm := range []string{realmGlobal, realmCN} {
		infos := catalogModelInfos(realm)
		if len(infos) == 0 {
			t.Fatalf("%s: empty catalog", realm)
		}
		for _, m := range infos {
			if m.ID == "" {
				t.Fatalf("%s: catalogModelInfos exposed an empty model ID", realm)
			}
			if m.ContextLength <= 0 || m.MaxCompletionTokens <= 0 {
				t.Fatalf("%s: model %s has non-positive token limits: %+v", realm, m.ID, m)
			}
			if m.OwnedBy != providerName {
				t.Fatalf("%s: model %s owned_by = %q", realm, m.ID, m.OwnedBy)
			}
		}
	}
}

// TestEffectiveSettingsLayering resolves default → realm → account.
func TestEffectiveSettingsLayering(t *testing.T) {
	useTempStateDir(t)

	// Layer 1: defaults.
	def := effectiveSettings(realmCN)
	if !def.Prefix.On || def.Prefix.Val != "cn/" {
		t.Fatalf("CN default prefix = %+v, want on/cn/", def.Prefix)
	}
	if g := effectiveSettings(realmGlobal); g.Prefix.On || g.Prefix.Val != "" {
		t.Fatalf("Global default prefix = %+v, want off/empty", g.Prefix)
	}

	// Layer 2: realm-wide settings replace the defaults.
	realmState := mmSettings{
		Disabled:      []string{"glm-5.3"},
		Custom:        []mmCustomModel{{ID: "my-model", Name: "Mine", Ctx: "128k"}},
		Prefix:        mmPrefixState{On: true, Val: "cn/x/"},
		Fallback:      mmFallbackState{On: true, Model: "glm-5.2"},
		RetryFallback: mmFallbackState{On: true, Model: "glm-5.1"},
	}
	if err := updateRealmSettings(realmCN, realmState); err != nil {
		t.Fatalf("updateRealmSettings: %v", err)
	}
	got := effectiveSettings(realmCN)
	if len(got.Disabled) != 1 || got.Disabled[0] != "glm-5.3" {
		t.Fatalf("realm disabled = %v", got.Disabled)
	}
	if got.Prefix.Val != "cn/x/" || !got.Fallback.On || got.Fallback.Model != "glm-5.2" || !got.RetryFallback.On {
		t.Fatalf("realm layer not applied: %+v", got)
	}
	if len(got.Custom) != 1 || got.Custom[0].ID != "my-model" {
		t.Fatalf("realm custom = %+v", got.Custom)
	}
	// A realm with no stored layer keeps the defaults.
	if g := effectiveSettings(realmGlobal); len(g.Disabled) != 0 {
		t.Fatalf("global should have no disabled models, got %v", g.Disabled)
	}
}

// TestResetRealmSettingsRestoresDefaults checks GET ?reset=1 semantics at the
// state layer: realm layer gone, defaults back.
func TestResetRealmSettingsRestoresDefaults(t *testing.T) {
	useTempStateDir(t)
	if err := updateRealmSettings(realmCN, mmSettings{
		Disabled: []string{"glm-5.3"},
		Prefix:   mmPrefixState{On: false},
	}); err != nil {
		t.Fatalf("updateRealmSettings: %v", err)
	}
	if err := resetRealmSettings(realmCN); err != nil {
		t.Fatalf("resetRealmSettings: %v", err)
	}
	got := effectiveSettings(realmCN)
	if !got.Prefix.On || got.Prefix.Val != "cn/" {
		t.Fatalf("after reset prefix = %+v, want on/cn/", got.Prefix)
	}
	if len(got.Disabled) != 0 {
		t.Fatalf("after reset disabled = %v, want empty", got.Disabled)
	}
	if err := resetRealmSettings("bogus"); err == nil {
		t.Fatal("reset of an unknown realm must fail")
	}
}

// TestStateFileAtomicSave verifies the on-disk contract, that saving leaves no
// temp files behind and that a reload reads the same values back.
func TestStateFileAtomicSave(t *testing.T) {
	dir := useTempStateDir(t)
	statePath := filepath.Join(dir, mmStateFileName)

	if err := updateRealmSettings(realmGlobal, mmSettings{
		Disabled: []string{"gpt-6-astra"},
		Prefix:   mmPrefixState{On: true, Val: "cb/"},
	}); err != nil {
		t.Fatalf("updateRealmSettings: %v", err)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var onDisk struct {
		Realm map[string]struct {
			Disabled []string `json:"disabled"`
			Custom   []struct {
				ID string `json:"id"`
			} `json:"custom"`
			Prefix struct {
				On  bool   `json:"on"`
				Val string `json:"val"`
			} `json:"prefix"`
			Fallback      map[string]any `json:"fallback"`
			RetryFallback map[string]any `json:"retry_fallback"`
		} `json:"realm"`
		Update map[string]any `json:"update"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("state file is not the contract JSON: %v\n%s", err, raw)
	}
	g, ok := onDisk.Realm[realmGlobal]
	if !ok {
		t.Fatalf("realm.global missing: %s", raw)
	}
	if len(g.Disabled) != 1 || g.Disabled[0] != "gpt-6-astra" {
		t.Fatalf("disabled = %v", g.Disabled)
	}
	if !g.Prefix.On || g.Prefix.Val != "cb/" {
		t.Fatalf("prefix = %+v", g.Prefix)
	}
	if g.Custom == nil {
		t.Fatal("custom must serialize as [] rather than null")
	}
	if _, ok := onDisk.Realm[realmCN]; ok {
		t.Fatal("saving the Global layer must not create a CN layer")
	}

	// No temp leftovers in the data dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("atomic save left a temp file: %s", e.Name())
		}
	}

	// A fresh process (cache cleared) must read the same values.
	resetModelsConfigCaches()
	got := effectiveSettings(realmGlobal)
	if len(got.Disabled) != 1 || got.Disabled[0] != "gpt-6-astra" || !got.Prefix.On {
		t.Fatalf("reloaded settings = %+v", got)
	}

	// The update section survives an unrelated save (WORKER-1c shares the file).
	if err := saveUpdateSeenVersion("0.9.6"); err != nil {
		t.Fatalf("saveUpdateSeenVersion: %v", err)
	}
	if err := updateRealmSettings(realmCN, mmSettings{Prefix: mmPrefixState{On: true, Val: "cn/"}}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if seen := loadUpdateState().SeenVersion; seen != "0.9.6" {
		t.Fatalf("update.seenVersion = %q after an unrelated save", seen)
	}
}

// TestStateFileCorruptRecovery: a damaged state file must not brick the panel.
func TestStateFileCorruptRecovery(t *testing.T) {
	dir := useTempStateDir(t)
	statePath := filepath.Join(dir, mmStateFileName)
	if err := os.WriteFile(statePath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	got := effectiveSettings(realmCN)
	if !got.Prefix.On {
		t.Fatalf("corrupt file should fall back to defaults, got %+v", got)
	}
	if _, err := os.Stat(statePath + ".corrupt"); err != nil {
		t.Fatalf("corrupt file was not moved aside: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Upstream discovery
// -----------------------------------------------------------------------------

// callRecorder collects the upstream requests a test fixture served. The two
// catalog fetches run concurrently, so the appends need a lock.
type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (c *callRecorder) add(v string) {
	c.mu.Lock()
	c.calls = append(c.calls, v)
	c.mu.Unlock()
}

func (c *callRecorder) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// fixtureServer serves a Global-like pair of catalogs:
//   - /v2/enterprises/personal/models: the official CLI list (glm-5.2 + tier)
//   - /v3/config: the full catalog, with extra models, a defaultLength that is
//     smaller than supportedLengths[-1], a util model and a disabled entry.
func fixtureServer(t *testing.T, rec *callRecorder) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/enterprises/personal/models", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path + " " + r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
			{"id":"default-model","name":"Auto","credits":"x0.79 credits","maxInputTokens":176000,"maxOutputTokens":24000,"maxAllowedSize":200000},
			{"id":"glm-5.2","name":"GLM-5.2","credits":"x0.79 credits","maxInputTokens":1000000,"maxOutputTokens":48000},
			{"id":"","name":"broken entry"},
			{"id":"gone-model","name":"Gone","disabled":true}
		]}}`))
	})
	mux.HandleFunc("/v3/config", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path + " " + r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
			{"id":"glm-5.2","name":"GLM-5.2","credits":"x0.79","vendor":"zhipu","maxInputTokens":1000000,"maxOutputTokens":48000,"contextWindow":{"defaultLength":300000,"supportedLengths":[300000,1000000]}},
			{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","credits":"x0.00","vendor":"f","maxInputTokens":1000000,"maxOutputTokens":128000,"contextWindow":{"defaultLength":300000,"supportedLengths":[300000,1000000]}},
			{"id":"kimi-k2.8-preview","name":"Kimi-K2.8-Preview","credits":"x0.77 credits","vendor":"moonshot","maxInputTokens":1000000,"maxOutputTokens":64000,"contextWindow":{"defaultLength":200000,"supportedLengths":[200000,1000000]}},
			{"id":"codewise-jump","name":"codewise-jump","maxOutputTokens":256,"vendor":"tencent"},
			{"id":"wb-lite","name":"wb-lite","disabled":true}
		]}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// withFakeFetchTarget replaces the host-auth lookup (unavailable outside a
// running CPA host). CN-prefixed AuthIDs resolve to the CN realm so a test can
// drive both realms at once.
func withFakeFetchTarget(t *testing.T, realm string) {
	t.Helper()
	origTarget := mmFetchTargetFn
	mmFetchTargetFn = func(authID string) (*mmFetchTarget, error) {
		r := realm
		if strings.HasPrefix(authID, "CN-") {
			r = realmCN
		}
		return &mmFetchTarget{AuthID: authID, Realm: r, Token: "tok", UID: "uid-1", Origin: "https://www.codebuddy.ai"}, nil
	}
	t.Cleanup(func() { mmFetchTargetFn = origTarget })
}

// TestFetchUpstreamModelsUnionDedup covers UA, union/dedup, /v3-wins fields,
// max-context (not defaultLength), credit normalization, hidden/util flags and
// the skipped entries.
func TestFetchUpstreamModelsUnionDedup(t *testing.T) {
	useTempStateDir(t)
	rec := &callRecorder{}
	srv := fixtureServer(t, rec)
	withFakeFetchTarget(t, realmGlobal)

	origGlobal, origCN := mmUpstreamBaseGlobal, mmUpstreamBaseCN
	mmUpstreamBaseGlobal, mmUpstreamBaseCN = srv.URL, srv.URL
	t.Cleanup(func() { mmUpstreamBaseGlobal, mmUpstreamBaseCN = origGlobal, origCN })

	models, err := fetchUpstreamModels("auth-1")
	if err != nil {
		t.Fatalf("fetchUpstreamModels: %v", err)
	}
	byID := map[string]upstreamModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if len(byID) != 5 {
		t.Fatalf("union = %d models (%v), want 5 (dedup + skip empty id + skip disabled)", len(byID), keysOf(byID))
	}
	if _, ok := byID[""]; ok {
		t.Fatal("entry without id must be skipped")
	}
	if _, ok := byID["gone-model"]; ok {
		t.Fatal("disabled upstream entry must be skipped")
	}
	if _, ok := byID["wb-lite"]; ok {
		t.Fatal("disabled /v3 entry must be skipped")
	}

	glm := byID["glm-5.2"]
	if glm.Mult != "x0.79" {
		t.Fatalf("glm-5.2 multiplier = %q, want x0.79", glm.Mult)
	}
	if glm.Ctx != 1000000 {
		t.Fatalf("glm-5.2 ctx = %d, want 1000000 (supportedLengths[-1], not defaultLength)", glm.Ctx)
	}
	if glm.MaxTok != 48000 {
		t.Fatalf("glm-5.2 maxTok = %d", glm.MaxTok)
	}
	if glm.Hidden {
		t.Fatal("glm-5.2 is in /v2 and in the catalog: must not be hidden")
	}

	// Tier alias keeps the catalog Tier flag and its route name.
	if auto := byID["default-model"]; !auto.Tier {
		t.Fatalf("default-model must be a tier alias: %+v", auto)
	}
	// A model only present in /v3/config is "hidden" on Global.
	ds := byID["deepseek-v4.1-flash"]
	if !ds.Hidden {
		t.Fatal("v3-only Global model must be flagged hidden")
	}
	if ds.Mult != "x0.00" {
		t.Fatalf("deepseek multiplier = %q", ds.Mult)
	}
	if ds.Ctx != 1000000 || ds.MaxTok != 128000 {
		t.Fatalf("deepseek ctx/maxTok = %d/%d", ds.Ctx, ds.MaxTok)
	}
	// Tiny-output internal model → utility.
	if util := byID["codewise-jump"]; !util.Util {
		t.Fatalf("codewise-jump must be util: %+v", util)
	}
	// "x0.77 credits" (v2 style suffix) normalizes the same way.
	if kimi := byID["kimi-k2.8-preview"]; kimi.Mult != "x0.77" {
		t.Fatalf("kimi multiplier = %q", kimi.Mult)
	}

	// Both endpoints were queried with the WorkBuddy UA.
	calls := rec.list()
	if len(calls) != 2 {
		t.Fatalf("expected 2 upstream calls, got %v", calls)
	}
	for _, c := range calls {
		if !strings.HasSuffix(c, " "+workBuddyUA) {
			t.Fatalf("call %q did not use the WorkBuddy UA", c)
		}
	}
}

func keysOf(m map[string]upstreamModel) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestFetchUpstreamModelsEnterpriseFallback: a 500 on /v2 must fall back to
// /console, and a total failure must surface an error (not an empty list).
func TestFetchUpstreamModelsEnterpriseFallback(t *testing.T) {
	useTempStateDir(t)
	rec := &callRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/enterprises/personal/models", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/console/enterprises/personal/models", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"legacy-only","name":"Legacy","credits":"x0.10"}]}}`))
	})
	mux.HandleFunc("/v3/config", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	withFakeFetchTarget(t, realmGlobal)
	origGlobal := mmUpstreamBaseGlobal
	mmUpstreamBaseGlobal = srv.URL
	t.Cleanup(func() { mmUpstreamBaseGlobal = origGlobal })

	models, err := fetchUpstreamModels("auth-1")
	if err != nil {
		t.Fatalf("fetchUpstreamModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "legacy-only" {
		t.Fatalf("fallback models = %+v", models)
	}
	if !containsString(rec.list(), "/console/enterprises/personal/models") {
		t.Fatalf("console fallback not tried: %v", rec.list())
	}

	// Both paths failing → error.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(failSrv.Close)
	mmUpstreamBaseGlobal = failSrv.URL
	if _, err := fetchUpstreamModels("auth-1"); err == nil {
		t.Fatal("expected an error when every upstream path fails")
	}
}

// TestUpstreamCacheAndInvalidate: the TTL cache avoids repeat fetches and
// invalidateUpstreamModels (POST /models/refresh) drops them.
func TestUpstreamCacheAndInvalidate(t *testing.T) {
	useTempStateDir(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2","credits":"x0.79"}]}}`))
	}))
	t.Cleanup(srv.Close)
	withFakeFetchTarget(t, realmGlobal)
	origGlobal := mmUpstreamBaseGlobal
	mmUpstreamBaseGlobal = srv.URL
	t.Cleanup(func() { mmUpstreamBaseGlobal = origGlobal })

	if _, _, err := upstreamModelsCached("auth-cache"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, _, err := upstreamModelsCached("auth-cache"); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 HTTP calls for 2 paths, got %d", got)
	}
	invalidateUpstreamModels("auth-cache")
	if _, _, err := upstreamModelsCached("auth-cache"); err != nil {
		t.Fatalf("fetch after invalidate: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("expected a refetch after invalidate, got %d calls", got)
	}
}

// -----------------------------------------------------------------------------
// Row building + endpoint contract
// -----------------------------------------------------------------------------

func TestBuildModelRowsFlagsAndOrder(t *testing.T) {
	upstream := []upstreamModel{
		{modelDef: modelDef{ID: "glm-5.3", Name: "GLM-5.3", Ctx: 1000000, Hidden: true}, Mult: "x0.79"},
		{modelDef: modelDef{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Ctx: 1000000}, Mult: "x0.00"},
		{modelDef: modelDef{ID: "gpt-5.4", Name: "GPT-5.4", Ctx: 272000}, Mult: "x1.65"},
		{modelDef: modelDef{ID: "default-model", Name: "Auto", Ctx: 176000, Tier: true}, Mult: ""},
	}
	rows := buildModelRows(realmGlobal, upstream, []mmCustomModel{{ID: "mine", Name: "Mine", Ctx: "128k"}})
	if rows[0].ID != "default-model" || !rows[0].Tier {
		t.Fatalf("tier rows must come first: %+v", rows[0])
	}
	byID := map[string]mmModelRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if r := byID["glm-5.3"]; !r.Hidden || r.Stale || r.Ctx != "1M" {
		t.Fatalf("glm-5.3 row = %+v", r)
	}
	// Catalog-only models are stale and carry the archived multiplier.
	if r := byID["glm-5.1"]; !r.Stale || r.Mult != "x0.79" {
		t.Fatalf("glm-5.1 row = %+v (want stale + archived x0.79)", r)
	}
	// Verified hidden models keep the measured multiplier.
	if r := byID["grok-4.6"]; !r.Verified || r.Mult != "x0.78" || !r.Hidden {
		t.Fatalf("grok-4.6 row = %+v", r)
	}
	// Models the catalog does not know are "new".
	if r := byID["gpt-5.4"]; !r.New {
		t.Fatalf("gpt-5.4 row = %+v (want new)", r)
	}
	if r := byID["glm-5.2"]; r.New {
		t.Fatalf("glm-5.2 is in the catalog, must not be new: %+v", r)
	}
	if r := byID["mine"]; !r.Custom || r.Mult != "?" {
		t.Fatalf("custom row = %+v", r)
	}
	// Vendor grouping + price order inside the upstream section.
	var seen []string
	for _, r := range rows {
		if r.Tier || r.Util || r.Stale || r.Verified || r.Custom {
			continue
		}
		seen = append(seen, r.ID)
	}
	want := []string{"deepseek-v4.1-flash", "glm-5.3", "gpt-5.4"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("upstream order = %v, want %v", seen, want)
	}
}

// TestModelsConfigEndpointGet covers the GET payload contract the panel reads.
func TestModelsConfigEndpointGet(t *testing.T) {
	useTempStateDir(t)
	rec := &callRecorder{}
	srv := fixtureServer(t, rec)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(dead.Close)
	withFakeFetchTarget(t, realmGlobal)
	origGlobal, origCN := mmUpstreamBaseGlobal, mmUpstreamBaseCN
	mmUpstreamBaseGlobal, mmUpstreamBaseCN = srv.URL, dead.URL
	t.Cleanup(func() { mmUpstreamBaseGlobal, mmUpstreamBaseCN = origGlobal, origCN })

	origAccounts := mmAccountsForRealmFn
	mmAccountsForRealmFn = func(realm string) []string {
		if realm == realmGlobal {
			return []string{"Intl-zxdrwin0602"}
		}
		return []string{"CN-64426370"}
	}
	t.Cleanup(func() { mmAccountsForRealmFn = origAccounts })

	payload := handleModelsConfigGet(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/workbuddy/models/config",
		Query:  map[string][]string{},
	})
	if err, ok := payload["error"]; ok {
		t.Fatalf("GET /models/config error: %v", err)
	}
	realms, ok := payload["realms"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no realms: %+v", payload)
	}
	global, ok := realms[realmGlobal].(map[string]any)
	if !ok {
		t.Fatalf("no global realm payload: %+v", realms)
	}
	for _, key := range []string{"accounts", "fetch_account", "fetched_at",
		"upstream", "catalog", "free", "prefix", "disabled", "custom", "fallback", "retry_fallback", "models"} {
		if _, ok := global[key]; !ok {
			t.Fatalf("global payload misses %q: %+v", key, global)
		}
	}
	if global["fetch_account"] != "Intl-zxdrwin0602" {
		t.Fatalf("fetch_account = %v", global["fetch_account"])
	}
	if global["upstream"] != 5 {
		t.Fatalf("upstream count = %v, want 5", global["upstream"])
	}
	prefix, _ := global["prefix"].(map[string]any)
	if prefix["enabled"] != false || prefix["def"] != "cb/" {
		t.Fatalf("global prefix = %+v", prefix)
	}
	cn, _ := realms[realmCN].(map[string]any)
	cnPrefix, _ := cn["prefix"].(map[string]any)
	if cnPrefix["enabled"] != true || cnPrefix["def"] != "cn/" {
		t.Fatalf("cn prefix = %+v", cnPrefix)
	}
	if cn["fetch_account"] != "CN-64426370" {
		t.Fatalf("cn fetch_account = %v", cn["fetch_account"])
	}
	// CN hits a dead upstream: the realm must report 0 models plus the fetch
	// error instead of pretending the catalog is empty.
	if cn["upstream"] != 0 {
		t.Fatalf("cn upstream = %v, want 0 with a dead upstream", cn["upstream"])
	}
	if _, ok := cn["fetch_error"]; !ok {
		t.Fatalf("cn payload must carry fetch_error: %+v", cn)
	}
	models, ok := global["models"].([]mmModelRow)
	if !ok || len(models) == 0 {
		t.Fatalf("global models = %#v", global["models"])
	}
	free := 0
	for _, m := range models {
		if m.Mult == "x0.00" {
			free++
		}
	}
	if global["free"] != free {
		t.Fatalf("free count = %v, want %d", global["free"], free)
	}
	if v, ok := payload["version"].(map[string]any); !ok || v["current"] != version {
		t.Fatalf("version payload = %+v", payload["version"])
	}
}

// TestModelsConfigEndpointPost covers the save/override/remove flow the panel
// drives, and that GET reflects the saved state.
func TestModelsConfigEndpointPost(t *testing.T) {
	dir := useTempStateDir(t)
	origAccounts := mmAccountsForRealmFn
	mmAccountsForRealmFn = func(realm string) []string { return []string{"auth-1"} }
	t.Cleanup(func() { mmAccountsForRealmFn = origAccounts })
	// No upstream: this test only exercises the state layer.
	withFakeFetchTarget(t, realmGlobal)

	post := func(body string) map[string]any {
		t.Helper()
		return handleModelsConfigPost(pluginapi.ManagementRequest{
			Method: http.MethodPost,
			Path:   "/v0/management/plugins/workbuddy/models/config",
			Query:  map[string][]string{},
			Body:   []byte(body),
		})
	}

	if out := post(`{"realm":"global","scope":"all","state":{"disabled":["gpt-6-astra"],"custom":[{"id":"mine","name":"Mine","ctx":"128k"}],"prefix_enabled":true,"prefix_value":"cb/","fallback_enabled":true,"fallback_model":"glm-5.2","retry_fallback_enabled":true,"retry_fallback_model":"glm-5.1"}}`); out["error"] != nil {
		t.Fatalf("save realm state: %v", out["error"])
	}
	got := effectiveSettings(realmGlobal)
	if len(got.Disabled) != 1 || !got.Prefix.On || !got.Fallback.On || !got.RetryFallback.On {
		t.Fatalf("realm state not saved: %+v", got)
	}
	if got.Fallback.Model != "glm-5.2" || got.RetryFallback.Model != "glm-5.1" {
		t.Fatalf("fallback models = %+v / %+v", got.Fallback, got.RetryFallback)
	}

	// The per-account layer is retired: scope=acct/auth is rejected and the
	// realm layer stays untouched.
	if out := post(`{"realm":"global","scope":"acct","auth":"auth-1","state":{"disabled":["hy3"]}}`); out["error"] == nil {
		t.Fatal("scope=acct must be rejected")
	}
	if g := effectiveSettings(realmGlobal); len(g.Disabled) != 1 || g.Disabled[0] != "gpt-6-astra" {
		t.Fatalf("rejected per-account save must not touch the realm layer: %+v", g)
	}
	payload := handleModelsConfigGet(pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/x/models/config",
		Query: map[string][]string{"realm": {"global"}},
	})
	global := payload["realms"].(map[string]any)[realmGlobal].(map[string]any)
	if _, has := global["override_auth"]; has {
		t.Fatalf("GET must not report override fields any more: %+v", global)
	}

	// Bad realm is rejected and nothing is written.
	if out := post(`{"realm":"mars","scope":"all"}`); out["error"] == nil {
		t.Fatal("unknown realm must be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, mmStateFileName)); err != nil {
		t.Fatalf("state file missing: %v", err)
	}
}

// TestModelsConfigResetEndpoint covers GET ?reset=1&realm=… through the handler.
func TestModelsConfigResetEndpoint(t *testing.T) {
	useTempStateDir(t)
	origAccounts := mmAccountsForRealmFn
	mmAccountsForRealmFn = func(realm string) []string { return nil }
	t.Cleanup(func() { mmAccountsForRealmFn = origAccounts })

	if err := updateRealmSettings(realmCN, mmSettings{Disabled: []string{"glm-5.3"}, Prefix: mmPrefixState{On: false}}); err != nil {
		t.Fatalf("seed realm state: %v", err)
	}
	out := handleModelsConfigGet(pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/x/models/config",
		Query: map[string][]string{"reset": {"1"}, "realm": {realmCN}},
	})
	if out["error"] != nil {
		t.Fatalf("reset: %v", out["error"])
	}
	cn := out["realms"].(map[string]any)[realmCN].(map[string]any)
	if len(cn["disabled"].([]string)) != 0 {
		t.Fatalf("reset did not clear disabled: %+v", cn["disabled"])
	}
	prefix := cn["prefix"].(map[string]any)
	if prefix["enabled"] != true || prefix["value"] != "cn/" {
		t.Fatalf("reset did not restore the CN prefix default: %+v", prefix)
	}
	// reset without a realm is a contract violation.
	if bad := handleModelsConfigGet(pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/x/models/config", Query: map[string][]string{"reset": {"1"}},
	}); bad["error"] == nil {
		t.Fatal("reset without realm must fail")
	}
	// The reset GET is a mutation → it must be guarded like a POST.
	if !modelsConfigResetRequest(pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/management/plugins/workbuddy/models/config/",
		Query: map[string][]string{"reset": {"1"}},
	}) {
		t.Fatal("reset GET must be flagged as mutating")
	}
	if modelsConfigResetRequest(pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/management/plugins/workbuddy/models/config",
	}) {
		t.Fatal("plain GET must not be flagged as mutating")
	}
}

func TestFormatCtx(t *testing.T) {
	cases := map[int64]string{
		0: "", 1000000: "1M", 500000: "500k", 176000: "176k", 131000: "131k", 272000: "272k",
		131072: "131k", 262144: "262k", 204800: "205k", 960000: "960k", 1500000: "1.5M",
	}
	for in, want := range cases {
		if got := formatCtx(in); got != want {
			t.Errorf("formatCtx(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParseCreditsMultiplier(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"x0.00":         "x0.00",
		"x0.79 credits": "x0.79",
		"X1.33":         "x1.33",
		"n/a":           "",
		"x":             "",
	}
	for in, want := range cases {
		if got := parseCreditsMultiplier(in); got != want {
			t.Errorf("parseCreditsMultiplier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUpstreamMaxCtx(t *testing.T) {
	withWindow := mmUpstreamModel{ContextWindow: json.RawMessage(`{"defaultLength":300000,"supportedLengths":[300000,1000000]}`), MaxInputTokens: 1000000}
	if got := upstreamMaxCtx(withWindow); got != 1000000 {
		t.Fatalf("supportedLengths max = %d, want 1000000", got)
	}
	numberWindow := mmUpstreamModel{ContextWindow: json.RawMessage(`200000`)}
	if got := upstreamMaxCtx(numberWindow); got != 200000 {
		t.Fatalf("numeric contextWindow = %d", got)
	}
	fallback := mmUpstreamModel{MaxInputTokens: 176000, MaxAllowedSize: 200000}
	if got := upstreamMaxCtx(fallback); got != 176000 {
		t.Fatalf("maxInputTokens fallback = %d, want 176000", got)
	}
	if got := upstreamMaxCtx(mmUpstreamModel{MaxAllowedSize: 200000}); got != 200000 {
		t.Fatalf("maxAllowedSize fallback = %d", got)
	}
}

// TestAtomicWriteFileKeepsOldContentOnFailure: a write to an unwritable dir
// must not touch the existing file.
func TestAtomicWriteFileKeepsOldContentOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, mmStateFileName)
	if err := atomicWriteFile(path, []byte("first")); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	if err := atomicWriteFile(path, []byte("second")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "second" {
		t.Fatalf("content = %q err = %v", raw, err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("unexpected temp file")
	}
	// Reads see a complete file even while a rewrite is in flight (rename is
	// atomic); sanity-check the mtime path is not used for caching.
	time.Sleep(5 * time.Millisecond)
	if err := atomicWriteFile(path, []byte("third")); err != nil {
		t.Fatalf("third write: %v", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "third" {
		t.Fatalf("content = %q", raw)
	}
}

// loadState builds a copy for callers; every top-level field must survive that
// copy. DailyReport and TrialClaimed were silently dropped, which made their
// persistence look broken while the data was on disk the whole time.
func TestLoadStateCarriesEveryTopLevelField(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
	when := time.Now().UTC().Truncate(time.Second)
	if err := mutateState(func(st *wbStateFile) error {
		st.DailyReport = &persistedDailyReport{
			When: when,
			Rows: map[string]persistedDailyRow{"a1": {Day: "2026-10-14", Score: 2, Lit: true, Status: "already-lit"}},
		}
		st.TrialClaimed = map[string]bool{"a1": true}
		st.Update.SeenVersion = "9.9.9"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := loadState()
	if err != nil {
		t.Fatal(err)
	}
	if got.DailyReport == nil || !got.DailyReport.When.Equal(when) {
		t.Fatalf("DailyReport lost in copy: %+v", got.DailyReport)
	}
	if row, ok := got.DailyReport.Rows["a1"]; !ok || row.Score != 2 || !row.Lit {
		t.Fatalf("DailyReport rows lost: %+v", got.DailyReport.Rows)
	}
	if !got.TrialClaimed["a1"] {
		t.Fatalf("TrialClaimed lost in copy: %+v", got.TrialClaimed)
	}
	if got.Update.SeenVersion != "9.9.9" {
		t.Fatalf("Update lost: %+v", got.Update)
	}
}
