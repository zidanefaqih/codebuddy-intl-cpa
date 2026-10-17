// update_check.go implements the "new version available" checker for the
// panel badge (PLAN-0907 decision 12). On GET /update/check (wired by W1d into
// handleManagement) it compares plugins[0].version from the GitHub release
// registry against the build version, caching the result for 6 hours.
//
// Hard requirements (PLAN-0907 brief W1c):
//   - Fetch the release-attached registry
//     (releases/latest/download/registry.json), NOT raw/main — the main-branch
//     copy is bumped ahead of the actual release and would announce versions
//     that cannot be installed yet.
//   - Silent fail: offline / GitHub down / bad JSON → available:false with
//     current filled in. The update check must never surface an error to the
//     panel or block any other feature.
//   - Cache 6h + on-demand force refresh (refresh=1).
//   - seenVersion persistence ("Skip this version") via the shared state API;
//     legacy standalone markers are migrated on first read.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// updateRegistryURL is the release-attached registry (NOT raw/main: the main
// copy is bumped at version-bump time, before the tag/CI release exists, so it
// can advertise an uninstallable version). Var (not const) so tests can point
// it at an httptest server.
var updateRegistryURL = "https://github.com/zidanefaqih/codebuddy-intl-cpa/releases/latest/download/registry.json"

const (
	updateCacheTTL         = 6 * time.Hour
	updateFetchTimeout     = 10 * time.Second
	updateFallbackFileName = "workbuddy-update.json" // legacy migration only
)

// updateRegistry mirrors the subset of registry.json schema v2 we need.
type updateRegistry struct {
	SchemaVersion int                `json:"schema_version"`
	Plugins       []updatePluginMeta `json:"plugins"`
}

type updatePluginMeta struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	ReleaseTag  string `json:"release_tag,omitempty"`
	Description string `json:"description,omitempty"`
	Repository  string `json:"repository,omitempty"`
}

// updateCheckResponse is the GET /update/check contract (PLAN-0907).
// W2's panel popover renders: current, latest, changelog (description),
// available badge; skip button posts nothing — it just records seenVersion.
type updateCheckResponse struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	CheckedAt string `json:"checkedAt"` // RFC3339; empty when never checked
	Changelog string `json:"changelog,omitempty"`
	Seen      bool   `json:"seen"` // latest == seenVersion (user pressed "Skip")
	// Failed is true when the last fetch attempt could not complete (offline,
	// GitHub down, bad JSON). Silent-fail contract: still HTTP 200, still
	// available:false — the panel just hides the badge.
	Failed bool `json:"failed,omitempty"`
}

// defaultUpdateChecker is the process-wide instance used by /update/check.
var defaultUpdateChecker = newUpdateChecker()

type updateChecker struct {
	mu sync.Mutex

	// cache
	cached    *updateCheckResponse
	cachedAt  time.Time
	checkedAt time.Time // last successful registry fetch (any caller)

	// seenVersion ("Skip this version"), persisted.
	seenVal    string
	seenLoaded bool
}

// newUpdateChecker builds a standalone checker (tests use fresh instances to
// avoid cross-test cache pollution).
func newUpdateChecker() *updateChecker { return &updateChecker{} }

// handleUpdateCheck serves GET /update/check. Query:
//
//	refresh=1  → bypass the 6h cache and fetch now (panel-open / manual check)
//	seen=v0.9.7→ record seenVersion ("Skip this version") and persist it
func handleUpdateCheck(req pluginapi.ManagementRequest) updateCheckResponse {
	force := req.Query.Get("refresh") == "1"
	if seen := strings.TrimSpace(req.Query.Get("seen")); seen != "" {
		defaultUpdateChecker.setSeenVersion(seen)
	}
	return defaultUpdateChecker.check(force, version)
}

// check returns the update status, consulting the 6h cache unless force is
// set. All failures are silent: the response always carries Current.
func (u *updateChecker) check(force bool, current string) updateCheckResponse {
	u.mu.Lock()
	defer u.mu.Unlock()

	if !force && u.cached != nil && time.Since(u.cachedAt) < updateCacheTTL {
		resp := *u.cached
		resp.Current = current
		resp.Seen = resp.Latest != "" && resp.Latest == u.persistedSeen()
		return resp
	}

	latest, tag, changelog, err := fetchUpdateRegistry()
	now := time.Now()
	if err != nil {
		// Silent fail (PLAN-0907 decision 12): never error out the panel.
		// Serve the last good cached copy if we have one, else current-only.
		if u.cached != nil {
			resp := *u.cached
			resp.Current = current
			resp.Failed = true
			resp.Seen = resp.Latest != "" && resp.Latest == u.persistedSeen()
			return resp
		}
		return updateCheckResponse{
			Current:   current,
			CheckedAt: now.UTC().Format(time.RFC3339),
			Failed:    true,
		}
	}

	u.checkedAt = now
	resp := updateCheckResponse{
		Current:   current,
		Latest:    latest,
		Available: isNewerVersion(latest, current),
		CheckedAt: now.UTC().Format(time.RFC3339),
		Changelog: changelog,
	}
	_ = tag // available for future "download" deep-link; not in the contract
	resp.Seen = resp.Latest != "" && resp.Latest == u.persistedSeen()
	u.cached = &resp
	u.cachedAt = now
	return resp
}

// setSeenVersion records "Skip this version" and persists it. Errors are
// swallowed (silent-fail family): skipping must not break the check itself.
func (u *updateChecker) setSeenVersion(v string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.seenVal = strings.TrimSpace(v)
	u.seenLoaded = true
	updateWriteSeenVersion(u.seenVal)
}

// seenVal lazily loads the persisted seenVersion (state file or fallback).
func (u *updateChecker) persistedSeen() string {
	if !u.seenLoaded {
		u.seenVal = updateReadSeenVersion()
		u.seenLoaded = true
	}
	return u.seenVal
}

// fetchUpdateRegistry downloads and parses the release registry. It returns
// (version, release_tag, description/changelog, error).
func fetchUpdateRegistry() (string, string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), updateFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateRegistryURL, nil)
	if err != nil {
		return "", "", "", err
	}
	// Plain browser-ish UA is fine (brief: "UA biasa"); GitHub release
	// downloads are not gated.
	req.Header.Set("User-Agent", "Mozilla/5.0 (WorkBuddy update check)")
	req.Header.Set("Accept", "application/json")

	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("registry fetch: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", "", err
	}
	var reg updateRegistry
	if err := json.Unmarshal(body, &reg); err != nil {
		return "", "", "", err
	}
	for _, p := range reg.Plugins {
		if p.ID == "workbuddy" && strings.TrimSpace(p.Version) != "" {
			return strings.TrimSpace(p.Version), p.ReleaseTag, p.Description, nil
		}
	}
	if len(reg.Plugins) > 0 && strings.TrimSpace(reg.Plugins[0].Version) != "" {
		// Brief says plugins[0].version; id match is preferred but don't fail
		// the whole check if the id was renamed.
		p := reg.Plugins[0]
		return strings.TrimSpace(p.Version), p.ReleaseTag, p.Description, nil
	}
	return "", "", "", errors.New("registry.json: no plugin version found")
}

// -----------------------------------------------------------------------------
// seenVersion persistence
// -----------------------------------------------------------------------------
//
// Use the same data dir, mutex and atomic writer as Model Manager. Old builds
// wrote to the auth-dir root (instead of .codebuddy_data); migrate that marker
// and legacy standalone files only when canonical state has no seenVersion.

type updateSeenFile struct {
	SeenVersion string `json:"seenVersion"`
}

// updateStateDir mirrors usageLogDir()'s resolution order so the seen marker
// survives restarts in the same persistent dir as the rest of plugin state.
func updateStateDir() string {
	if d := strings.TrimSpace(os.Getenv("WORKBUDDY_DATA_DIR")); d != "" {
		return d
	}
	if files, err := hostAuthList(); err == nil {
		for _, f := range files {
			p := strings.TrimSpace(f.Path)
			if p == "" {
				continue
			}
			dir := filepath.Dir(p)
			if dir != "" && dir != "." && dir != string(filepath.Separator) {
				return dir
			}
		}
	}
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	return os.TempDir()
}

func updateWriteSeenVersion(v string) {
	_ = saveUpdateSeenVersion(v) // silent fail; never use an independent writer
}

func updateReadSeenVersion() string {
	if v := loadUpdateState().SeenVersion; v != "" {
		return v
	}
	for _, dir := range []string{wbDataDir(), updateStateDir()} {
		for _, name := range []string{mmStateFileName, updateFallbackFileName} {
			path := filepath.Join(dir, name)
			if path == wbStatePath() {
				continue
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var f struct {
				Update      updateSeenFile `json:"update"`
				SeenVersion string         `json:"seenVersion"`
			}
			if json.Unmarshal(raw, &f) != nil {
				continue
			}
			v := strings.TrimSpace(firstNonEmpty(f.Update.SeenVersion, f.SeenVersion))
			if v == "" {
				continue
			}
			// A concurrent canonical writer wins over a migrated legacy value.
			if err := mutateState(func(st *wbStateFile) error {
				if st.Update.SeenVersion == "" {
					st.Update.SeenVersion = v
				}
				return nil
			}); err == nil {
				return loadUpdateState().SeenVersion
			}
			return v
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// Version comparison
// -----------------------------------------------------------------------------

// isNewerVersion reports whether candidate is strictly newer than current.
// Semver-ish: numeric dot-separated comparison with v-prefix tolerance
// ("v0.9.7" == "0.9.7"), missing segments = 0 ("0.9" < "0.9.1"), non-numeric
// tails compared as strings last resort. Returns false for equal or older, so
// re-publishing the same version never re-triggers the badge.
func isNewerVersion(candidate, current string) bool {
	cand := strings.TrimPrefix(strings.TrimSpace(candidate), "v")
	cur := strings.TrimPrefix(strings.TrimSpace(current), "v")
	if cand == "" || cand == cur {
		return false
	}
	candParts := strings.Split(cand, ".")
	curParts := strings.Split(cur, ".")
	n := len(candParts)
	if len(curParts) > n {
		n = len(curParts)
	}
	for i := 0; i < n; i++ {
		var cs, hs string
		if i < len(candParts) {
			cs = candParts[i]
		}
		if i < len(curParts) {
			hs = curParts[i]
		}
		cn, cerr := strconvAtoi(cs)
		hn, herr := strconvAtoi(hs)
		switch {
		case cerr == nil && herr == nil:
			if cn != hn {
				return cn > hn
			}
		default:
			// Non-numeric segment (e.g. "0.10.0-rc1"): plain string compare.
			if cs != hs {
				return cs > hs
			}
		}
	}
	return false
}

// strconvAtoi is a tiny local atoi avoiding an extra import in hot paths;
// returns an error for empty or non-numeric input.
func strconvAtoi(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("non-numeric")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}
