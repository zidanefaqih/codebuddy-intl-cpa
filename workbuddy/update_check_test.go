// update_check_test.go — light tests for the W1c update checker (PLAN-0907):
// registry parsing, version comparison, cache TTL, silent fail, seenVersion
// persistence. No network: fetchUpdateRegistry is exercised via httptest by
// swapping updateRegistryURL.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func registryJSON(versions ...string) []byte {
	plugins := make([]updatePluginMeta, 0, len(versions))
	for _, v := range versions {
		plugins = append(plugins, updatePluginMeta{ID: "workbuddy", Version: v, Description: "rel " + v})
	}
	b, _ := json.Marshal(updateRegistry{SchemaVersion: 2, Plugins: plugins})
	return b
}

// serveRegistry points updateRegistryURL at an httptest server and restores it
// after the test.
func serveRegistry(t *testing.T, status int, body []byte) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	oldURL := updateRegistryURL
	updateRegistryURL = srv.URL
	t.Cleanup(func() { updateRegistryURL = oldURL })
}

func freshChecker(t *testing.T) *updateChecker {
	t.Helper()
	c := newUpdateChecker()
	c.seenLoaded = true // don't touch disk unless a test asks for it
	return c
}

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		cand, cur string
		want      bool
	}{
		{"0.9.7", "0.9.6", true},
		{"v0.9.7", "0.9.6", true}, // v-prefix tolerance
		{"0.9.6", "0.9.6", false}, // equal never re-triggers badge
		{"0.9.6", "0.9.7", false},
		{"0.9.5", "0.9.6", false},
		{"0.10.0", "0.9.9", true}, // numeric, not lexicographic
		{"0.9", "0.9.1", false},   // missing segment = 0
		{"0.9.1", "0.9", true},
		{"1.0", "0.9.9", true},
		{"", "0.9.6", false}, // empty candidate → no update
	}
	for _, tc := range cases {
		if got := isNewerVersion(tc.cand, tc.cur); got != tc.want {
			t.Errorf("isNewerVersion(%q, %q) = %v, want %v", tc.cand, tc.cur, got, tc.want)
		}
	}
}

func TestFetchUpdateRegistryParsesPlugins0(t *testing.T) {
	serveRegistry(t, http.StatusOK, registryJSON("0.9.7"))
	v, tag, desc, err := fetchUpdateRegistry()
	if err != nil {
		t.Fatalf("fetchUpdateRegistry: %v", err)
	}
	if v != "0.9.7" || desc != "rel 0.9.7" || tag != "" {
		t.Fatalf("got v=%q tag=%q desc=%q", v, tag, desc)
	}
}

func TestFetchUpdateRegistrySilentFail(t *testing.T) {
	// GitHub down → error surfaced to caller, but check() must stay silent.
	serveRegistry(t, http.StatusInternalServerError, []byte("boom"))
	if _, _, _, err := fetchUpdateRegistry(); err == nil {
		t.Fatal("expected error on HTTP 500")
	}

	c := freshChecker(t)
	resp := c.check(false, "0.9.6")
	if resp.Available || resp.Latest != "" || resp.Current != "0.9.6" || !resp.Failed {
		t.Fatalf("silent fail violated: %+v", resp)
	}
	if resp.CheckedAt == "" {
		t.Error("CheckedAt should still be stamped on failure")
	}

	// Bad JSON is also a silent fail.
	serveRegistry(t, http.StatusOK, []byte("not json{"))
	c2 := freshChecker(t)
	if r := c2.check(false, "0.9.6"); r.Available || !r.Failed {
		t.Fatalf("bad json should silently fail: %+v", r)
	}
}

func TestUpdateCheckAvailable(t *testing.T) {
	serveRegistry(t, http.StatusOK, registryJSON("0.9.7"))
	c := freshChecker(t)
	resp := c.check(false, "0.9.6")
	if !resp.Available || resp.Latest != "0.9.7" || resp.Current != "0.9.6" {
		t.Fatalf("expected available: %+v", resp)
	}
	if resp.Changelog != "rel 0.9.7" {
		t.Errorf("changelog: got %q", resp.Changelog)
	}
	// Same version → no badge.
	if r := c.check(true, "0.9.7"); r.Available {
		t.Fatalf("same version flagged available: %+v", r)
	}
}

func TestUpdateCheckCacheTTL(t *testing.T) {
	serveRegistry(t, http.StatusOK, registryJSON("0.9.7"))
	c := freshChecker(t)
	first := c.check(false, "0.9.6")
	if !first.Available {
		t.Fatalf("first check: %+v", first)
	}

	// Registry moves on, but cached response must win within TTL.
	serveRegistry(t, http.StatusOK, registryJSON("0.9.8"))
	cached := c.check(false, "0.9.6")
	if cached.Latest != "0.9.7" {
		t.Fatalf("cache not used within TTL: %+v", cached)
	}

	// Expire the cache → next check sees the new version.
	c.cachedAt = time.Now().Add(-updateCacheTTL - time.Second)
	refreshed := c.check(false, "0.9.6")
	if refreshed.Latest != "0.9.8" {
		t.Fatalf("TTL expiry ignored: %+v", refreshed)
	}

	// On-demand refresh bypasses the cache even when fresh.
	serveRegistry(t, http.StatusOK, registryJSON("0.9.9"))
	forced := c.check(true, "0.9.6")
	if forced.Latest != "0.9.9" {
		t.Fatalf("force refresh ignored: %+v", forced)
	}
}

func TestUpdateCheckConcurrent(t *testing.T) {
	serveRegistry(t, http.StatusOK, registryJSON("0.9.7"))
	c := freshChecker(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.check(i%2 == 0, "0.9.6")
		}()
	}
	wg.Wait()
}

func TestSeenVersionPersistence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WORKBUDDY_DATA_DIR", dir)

	// First write uses canonical shared state, even before a Model Manager save.
	updateWriteSeenVersion("0.9.7")
	if got := updateReadSeenVersion(); got != "0.9.7" {
		t.Fatalf("fallback read: got %q", got)
	}
	if _, err := os.Stat(wbStatePath()); err != nil {
		t.Fatalf("canonical state missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, updateFallbackFileName)); !os.IsNotExist(err) {
		t.Fatalf("must not create an independent fallback writer: %v", err)
	}

	// W1a state file present → key update.seenVersion inside it, fallback untouched.
	statePath := wbStatePath()
	state := map[string]any{
		"realm": map[string]any{"global": map[string]any{"disabled": []string{"m1"}}},
	}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	updateWriteSeenVersion("0.9.8")
	if got := updateReadSeenVersion(); got != "0.9.8" {
		t.Fatalf("state read: got %q", got)
	}
	var after map[string]any
	raw, _ = os.ReadFile(statePath)
	if json.Unmarshal(raw, &after) != nil {
		t.Fatal("state file corrupted")
	}
	if _, ok := after["realm"].(map[string]any); !ok {
		t.Fatal("state file lost sibling keys")
	}

	// Skip flow: latest==seen → Seen=true and Available stays for the record.
	// Fresh data dir + fresh checker so nothing leaks from the steps above.
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
	serveRegistry(t, http.StatusOK, registryJSON("0.9.8"))
	c := newUpdateChecker()
	resp := c.check(false, "0.9.6")
	if !resp.Available || resp.Seen {
		t.Fatalf("pre-skip: %+v", resp)
	}
	c.setSeenVersion("0.9.8")
	skipped := c.check(true, "0.9.6")
	if !skipped.Seen {
		t.Fatalf("post-skip: %+v", skipped)
	}
}

func TestHandleUpdateCheckQueryContract(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
	// 0.9.8: must always be newer than the built version (0.9.7+) so this test
	// does not flip when the release tag catches up with the mock.
	serveRegistry(t, http.StatusOK, registryJSON("0.9.8"))

	c := newUpdateChecker()
	oldChecker := defaultUpdateChecker
	defaultUpdateChecker = c
	t.Cleanup(func() { defaultUpdateChecker = oldChecker })

	u, _ := url.Parse("/update/check?refresh=1")
	req := pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/update/check", Query: u.Query()}
	resp := handleUpdateCheck(req)
	if !resp.Available || resp.Latest != "0.9.8" {
		t.Fatalf("refresh=1: %+v", resp)
	}

	// seen=0.9.7 records the skip; refresh=1 bypasses cache so Seen reflects it.
	u2, _ := url.Parse("/update/check?refresh=1&seen=0.9.8")
	resp2 := handleUpdateCheck(pluginapi.ManagementRequest{Query: u2.Query()})
	if !resp2.Seen {
		t.Fatalf("seen= not honored: %+v", resp2)
	}
}
