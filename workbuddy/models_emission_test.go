// Tests for CB-039: /v1/models emission must carry the same union the Model
// Manager panel shows (dynamic/static ∪ catalog ∪ upstream ∪ custom).
package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// stubEmissionUpstream swaps the per-account upstream catalog source.
func stubEmissionUpstream(t *testing.T, byAuth map[string][]upstreamModel) {
	t.Helper()
	prev := mmEmissionUpstreamFn
	mmEmissionUpstreamFn = func(authID string) []upstreamModel { return byAuth[authID] }
	t.Cleanup(func() { mmEmissionUpstreamFn = prev })
}

func emittedIDs(t *testing.T, raw []byte) map[string]bool {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	var resp pluginapi.ModelResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode result: %v (%s)", err, raw)
	}
	ids := make(map[string]bool, len(resp.Models))
	for _, m := range resp.Models {
		ids[m.ID] = true
	}
	if len(ids) == 0 {
		t.Fatalf("no models emitted: %s", raw)
	}
	return ids
}

func modelForAuthRequest(t *testing.T, authID string, sa storedAuth) []byte {
	t.Helper()
	storage, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(pluginapi.AuthModelRequest{AuthID: authID, StorageJSON: storage})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestEmissionIncludesUpstreamOnlyModels: a Global upstream-only model
// (gpt-6.1-sol, hidden) reaches /v1/models; utility models stay out; custom
// models are emitted; disabled + prefix still apply.
func TestEmissionIncludesUpstreamOnlyModels(t *testing.T) {
	useTempStateDir(t)
	const auth = "workbuddy-g1.json"
	stubEmissionUpstream(t, map[string][]upstreamModel{
		auth: {
			{modelDef: modelDef{ID: "gpt-6.1-sol", Name: "GPT-6.1-Sol", Realm: realmGlobal, Hidden: true}},
			{modelDef: modelDef{ID: "grok-4.7", Name: "Grok-4.7", Realm: realmGlobal}},
			{modelDef: modelDef{ID: "codewise-jump", Name: "codewise-jump", Realm: realmGlobal, Util: true}},
		},
	})
	if err := updateRealmSettings(realmGlobal, mmSettings{
		Disabled: []string{"grok-4.7"},
		Custom:   []mmCustomModel{{ID: "my-custom", Name: "Mine"}},
		Prefix:   mmPrefixState{On: true},
	}); err != nil {
		t.Fatal(err)
	}
	sa := storedAuth{Auth: storedTokens{AccessToken: "test-token", Domain: "https://www.codebuddy.ai"}, Account: storedAccount{UID: "g1"}}
	out, err := handleModelForAuth(modelForAuthRequest(t, auth, sa))
	if err != nil {
		t.Fatal(err)
	}
	ids := emittedIDs(t, out)
	for _, want := range []string{"cb/gpt-6.1-sol", "cb/my-custom"} {
		if !ids[want] {
			t.Errorf("missing %s in emission: %v", want, ids)
		}
	}
	for _, bad := range []string{"cb/grok-4.7", "cb/codewise-jump", "codewise-jump", "gpt-6.1-sol"} {
		if ids[bad] {
			t.Errorf("%s must not be emitted: %v", bad, ids)
		}
	}
	// static catalog still present (union, not replacement)
	if !ids["cb/gpt-6-astra"] {
		t.Errorf("static catalog model cb/gpt-6-astra dropped: %v", ids)
	}
}

// TestEmissionUpstreamPerAccount: upstream catalogs are per account — a peer
// without that model (never fetched) does not emit it.
func TestEmissionUpstreamPerAccount(t *testing.T) {
	useTempStateDir(t)
	stubEmissionUpstream(t, map[string][]upstreamModel{
		"workbuddy-a.json": {{modelDef: modelDef{ID: "gpt-6.1-sol", Realm: realmGlobal}}},
	})
	sa := storedAuth{Auth: storedTokens{AccessToken: "test-token", Domain: "https://www.codebuddy.ai"}}
	outA, _ := handleModelForAuth(modelForAuthRequest(t, "workbuddy-a.json", sa))
	outB, _ := handleModelForAuth(modelForAuthRequest(t, "workbuddy-b.json", sa))
	if !emittedIDs(t, outA)["gpt-6.1-sol"] {
		t.Fatal("account a must emit its upstream model")
	}
	if emittedIDs(t, outB)["gpt-6.1-sol"] {
		t.Fatal("account b never fetched gpt-6.1-sol and must not emit it")
	}
}

// TestUpstreamModelsForEmissionKeepsLastGood: a failed refresh keeps the last
// good catalog instead of shrinking the registry.
func TestUpstreamModelsForEmissionKeepsLastGood(t *testing.T) {
	const auth = "workbuddy-stale.json"
	mmFetchCache.mu.Lock()
	mmFetchCache.entries[auth] = mmFetchCacheEntry{models: []upstreamModel{{modelDef: modelDef{ID: "gpt-6.1-sol"}}}}
	mmFetchCache.mu.Unlock()
	t.Cleanup(func() { invalidateUpstreamModels(auth) })
	prev := mmFetchTargetFn
	mmFetchTargetFn = func(string) (*mmFetchTarget, error) { return nil, errors.New("upstream unavailable (test)") }
	t.Cleanup(func() { mmFetchTargetFn = prev })

	got := upstreamModelsForEmission(auth)
	if len(got) != 1 || !strings.EqualFold(got[0].ID, "gpt-6.1-sol") {
		t.Fatalf("want last good catalog, got %+v", got)
	}
	if upstreamModelsForEmission("workbuddy-never.json") != nil {
		t.Fatal("never-fetched account must return nil")
	}
}

// TestDropLegacyOverrides: a state file written before the per-account layer
// was retired loses "authOverride" on startup while the realm layer and the
// update section survive; a clean file is left untouched.
func TestDropLegacyOverrides(t *testing.T) {
	useTempStateDir(t)
	legacy := `{"realm":{"global":{"disabled":["hy3"],"custom":[],"prefix":{"on":true,"val":""},"fallback":{"on":false,"model":""},"retry_fallback":{"on":false,"model":""}}},` +
		`"authOverride":{"workbuddy-x.json":{"disabled":["glm-5.3"],"prefix":{"on":true,"val":""}}},"update":{"seenVersion":"0.9.6"}}`
	if err := atomicWriteFile(wbStatePath(), []byte(legacy)); err != nil {
		t.Fatal(err)
	}
	if n := mmDropLegacyOverrides(); n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	raw, err := os.ReadFile(wbStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "authOverride") {
		t.Fatalf("authOverride still on disk: %s", raw)
	}
	resetModelsConfigCaches()
	g := effectiveSettings(realmGlobal)
	if len(g.Disabled) != 1 || g.Disabled[0] != "hy3" || !g.Prefix.On {
		t.Fatalf("realm layer lost: %+v", g)
	}
	st, _ := loadState()
	if st.Update.SeenVersion != "0.9.6" {
		t.Fatalf("update section lost: %+v", st.Update)
	}
	if n := mmDropLegacyOverrides(); n != 0 {
		t.Fatalf("second run dropped %d, want 0", n)
	}
}
