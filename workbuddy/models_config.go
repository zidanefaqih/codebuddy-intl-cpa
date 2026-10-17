// models_config.go implements the Model Manager backend for v0.9.7
// (PLAN-0907, WORKER-1a):
//
//  1. The per-realm model catalog (modelDef): one source for the legacy
//     wbModels()/wbCNModels() entries plus the tier aliases, hidden/verified
//     models and the utility/internal models the panel renders.
//  2. Upstream catalog discovery per account: Global /v2/enterprises/personal/models
//     ∪ /v3/config, CN copilot.tencent.com /console/enterprises/personal/models
//     ∪ /v3/config, with the WorkBuddy client UA. The context window is the
//     *max* supported length (contextWindow.supportedLengths[-1]), never
//     contextWindow.defaultLength (decision 6 in PLAN-0907).
//  3. The plugin state file workbuddy-state.json (atomic save) with the
//     default → realm → per-account layering resolved by effectiveSettings.
//  4. The GET/POST /models/config management endpoint (+ ?reset=1).
//
// Contract: PLAN-0907 "Kontrak endpoint" (frozen). The response shape is the
// one the approved panel mockup (mockups/model-settings-mockup.html, WORKER-2)
// renders.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	realmGlobal = "global"
	realmCN     = "cn"

	// workBuddyUA is the client UA that unlocks the full /v3/config catalog.
	// With the plain CLI UA the endpoint returns fewer models; with the
	// WorkBuddy UA Global /v3/config returned 31 models instead of 22
	// (CB-024, PRICING-model-multiplier, 2026-10-13).
	workBuddyUA = "WorkBuddy/2.63.2 WorkBuddy AI/2.63.2 CLI/2.63.2"

	mmStateFileName = "workbuddy-state.json"
	mmFetchTimeout  = 20 * time.Second
	mmFetchTTL      = 5 * time.Minute
)

// -----------------------------------------------------------------------------
// Catalog
// -----------------------------------------------------------------------------

// modelDef is one entry of the plugin's own per-realm model catalog.
// Ctx/MaxTok are token counts (Ctx = maximum context, never the upstream
// default length). Hidden marks models the upstream does not advertise in its
// official CLI list; Tier marks the Auto/Fast/Balanced/... route aliases;
// Util marks internal/tool models that have no public chat pricing.
type modelDef struct {
	ID     string
	Name   string
	Realm  string
	Ctx    int64
	MaxTok int64
	Hidden bool
	Tier   bool
	Util   bool
}

// wbCatalogMeta holds the catalog entries the legacy per-realm tables do not
// have: the tier aliases (route tiers, not physical models) and the hidden
// models that were verified with a real test chat on 2026-10-13 (multipliers
// measured from the credit usage of those calls). Realm is filled in by
// catalogModels.
var wbCatalogMeta = map[string][]modelDef{
	realmGlobal: {
		{ID: "default-model", Name: "Auto", Ctx: 176000, MaxTok: 24000, Tier: true},
		{ID: "fast-model", Name: "Fast", Ctx: 200000, MaxTok: 32000, Tier: true},
		{ID: "balanced-model", Name: "Balanced", Ctx: 256000, MaxTok: 32000, Tier: true},
		{ID: "primary-model", Name: "Primary", Ctx: 272000, MaxTok: 72000, Tier: true},
		{ID: "deep-model", Name: "Deep", Ctx: 176000, MaxTok: 24000, Tier: true},
		{ID: "grok-4.6", Name: "Grok 4.6", Ctx: 500000, MaxTok: 128000, Hidden: true},
		{ID: "gemini-3.1-pro", Name: "Gemini 3.1 Pro", Ctx: 1000000, MaxTok: 128000, Hidden: true},
	},
	realmCN: {
		{ID: "fast-model", Name: "Fast", Ctx: 300000, MaxTok: 48000, Tier: true},
		{ID: "balanced-model", Name: "Balanced", Ctx: 300000, MaxTok: 48000, Tier: true},
		{ID: "deep-model", Name: "Deep", Ctx: 300000, MaxTok: 48000, Tier: true},
	},
}

// hiddenLegacyIDs marks the legacy catalog entries the upstream does not
// advertise in its official CLI list (the "hidden models" block of wbModels).
var hiddenLegacyIDs = map[string]bool{
	"global/gpt-5.6-luna":        true,
	"global/gpt-5.6-sol":         true,
	"global/gpt-5.6-terra":       true,
	"global/glm-5.3":             true,
	"global/deepseek-v4.1-flash": true,
	"global/glm-5.3-flash":       true,
	"global/gpt-6-astra":         true,
}

// legacyHiddenSignature marks the rest of the legacy hidden block: every entry
// of it shares one signature (200k context / 16k output) and one of those
// entries is not spelled out in the plan/mockup documents, so the signature
// keeps the static Hidden flag faithful without hardcoding it here.
func legacyHiddenSignature(d modelDef) bool {
	return d.Ctx == 200000 && d.MaxTok == 16384
}

// catalogModels returns the per-realm model catalog: the legacy
// wbModels()/wbCNModels() tables converted to modelDef plus the metadata
// entries above.
//
// The legacy tables still live in models.go — WORKER-1a must not touch that
// file (it belongs to WORKER-1d), and copying their entries by hand would
// create a second copy that can drift. Converting them here keeps exactly one
// API and one set of values; WORKER-1d inlines the converted table into this
// file when it deletes the legacy functions, and TestCatalogCoversLegacyEntries
// locks the values until then.
func catalogModels(realm string) []modelDef {
	if realm != realmCN {
		realm = realmGlobal
	}
	legacy := wbModels()
	if realm == realmCN {
		legacy = wbCNModels()
	}
	out := make([]modelDef, 0, len(legacy)+len(wbCatalogMeta[realm]))
	seen := make(map[string]bool, len(legacy))
	for _, m := range legacy {
		seen[m.ID] = true
		d := modelDef{
			ID:     m.ID,
			Name:   m.Name,
			Realm:  realm,
			Ctx:    m.ContextLength,
			MaxTok: m.MaxCompletionTokens,
			Hidden: hiddenLegacyIDs[realm+"/"+m.ID],
		}
		if legacyHiddenSignature(d) {
			d.Hidden = true
		}
		out = append(out, d)
	}
	for _, d := range wbCatalogMeta[realm] {
		if seen[d.ID] {
			continue
		}
		d.Realm = realm
		out = append(out, d)
	}
	return out
}

// catalogModel returns the catalog entry for an ID, if any.
func catalogModel(realm, id string) (modelDef, bool) {
	for _, d := range catalogModels(realm) {
		if d.ID == id {
			return d, true
		}
	}
	return modelDef{}, false
}

// catalogModelInfos converts the catalog into the host's ModelInfo shape
// (routing/registration use; prefix handling stays in models.go).
func catalogModelInfos(realm string) []pluginapi.ModelInfo {
	defs := catalogModels(realm)
	out := make([]pluginapi.ModelInfo, 0, len(defs))
	for _, d := range defs {
		if d.ID == "" {
			// Legacy placeholder entry ("the model 5") has no usable ID.
			continue
		}
		out = append(out, modelDefInfo(d))
	}
	return out
}

func modelDefInfo(d modelDef) pluginapi.ModelInfo {
	return pluginapi.ModelInfo{
		ID:                         d.ID,
		Name:                       d.Name,
		ContextLength:              d.Ctx,
		MaxCompletionTokens:        d.MaxTok,
		OwnedBy:                    providerName,
		SupportedGenerationMethods: []string{"chat"},
	}
}

// -----------------------------------------------------------------------------
// Static side tables (catalog metadata that is not part of modelDef)
// -----------------------------------------------------------------------------

// archivedMultipliers holds the credit multiplier last seen for models that the
// upstream catalog no longer returns (panel renders them with the "~" prefix:
// archived, possibly stale) plus the multipliers measured with real test chats
// on 2026-10-13 for the verified hidden models. Key: realm + "/" + id.
var archivedMultipliers = map[string]string{
	"global/glm-5.1":           "x0.79",
	"global/glm-5v-turbo":      "x0.71",
	"global/kimi-k2.7":         "x0.57",
	"global/minimax-m3":        "x0.12",
	"global/deepseek-v4-pro":   "x0.51",
	"global/deepseek-v4-flash": "x0.17",
	"global/grok-4.6":          "x0.78",
	"global/gemini-3.1-pro":    "x0.88",
	"cn/deepseek-v4-pro":       "x0.51",
	"cn/kimi-k2.7":             "x0.57",
}

// verifiedModelIDs marks models that are not part of the upstream catalog but
// were verified with a real test chat (panel badge "hidden·verified").
var verifiedModelIDs = map[string]bool{
	"global/grok-4.6":       true,
	"global/gemini-3.1-pro": true,
	"global/minimax-m3":     true,
}

// effortModelIDs marks models that answer but reject the default
// reasoning_effort (upstream code 11150) — panel badge "effort?".
var effortModelIDs = map[string]bool{
	"cn/glm-5.0-turbo": true,
}

// utilModelIDs lists the internal/tool models the CN catalog advertises. They
// have no public chat pricing, so the panel shows them in the utility section
// (default OFF).
var utilModelIDs = map[string]bool{
	"codewise-completions":             true,
	"codewise-jump":                    true,
	"codewise-nes-a4-027-aide":         true,
	"codewise-rewrite":                 true,
	"completion-gf":                    true,
	"deepseek-r1-0528":                 true,
	"deepseek-r1-0528-lkeap":           true,
	"deepseek-v3-0324":                 true,
	"deepseek-v3-0324-taco-completion": true,
	"default-1.1":                      true,
	"default-1.2":                      true,
	"hunyuan-2.0-instruct":             true,
	"hunyuan-3b":                       true,
	"hunyuan-7b-dense":                 true,
	"hunyuan-chat":                     true,
	"hunyuan-image-alpha":              true,
	"hunyuan-image-alpha-edit":         true,
	"kimi-k2-instruct-taiji":           true,
	"wb-lite":                          true,
}

// isUtilModelID reports whether an upstream entry is an internal/tool model:
// known utility IDs, the non-chat ID prefixes used by the CN harness
// (nes-/completion-/codewise-) and tiny output models (<=256 tokens) that can
// only serve completion-style requests.
func isUtilModelID(id string, maxOut int64) bool {
	if utilModelIDs[id] {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(id))
	for _, p := range []string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return maxOut > 0 && maxOut <= 256
}

// -----------------------------------------------------------------------------
// Upstream catalog discovery
// -----------------------------------------------------------------------------

// upstreamModel is one model discovered upstream, already resolved to the
// catalog shape the panel needs.
type upstreamModel struct {
	modelDef
	Mult   string // "x0.14"; empty when the upstream omits the credits field
	Vendor string
}

// mmFetchTarget is the per-account context of an upstream fetch.
type mmFetchTarget struct {
	AuthID string
	Realm  string
	Token  string
	UID    string
	Origin string
}

// Seams for tests: the auth lookup, the two upstream bases and the HTTP
// transport (host bridge in production, plain client in tests).
var (
	mmFetchTargetFn      = resolveFetchTarget
	mmUpstreamBaseGlobal = upstreamBaseGlobal
	mmUpstreamBaseCN     = upstreamBaseCN
	mmModelsHTTPDo       = hostHTTPDo
)

// resolveFetchTarget loads the credential behind an AuthID (or AuthIndex) and
// derives realm, token and Origin/Referer for the model endpoints.
func resolveFetchTarget(authID string) (*mmFetchTarget, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, fmt.Errorf("empty auth id")
	}
	files, err := hostAuthList()
	if err != nil {
		return nil, fmt.Errorf("list auth: %w", err)
	}
	for _, f := range files {
		if f.ID != authID && f.AuthIndex != authID {
			continue
		}
		sa, _, err := hostAuthGetBundle(f.AuthIndex)
		if err != nil {
			return nil, fmt.Errorf("load auth %s: %w", authID, err)
		}
		token := strings.TrimSpace(sa.Auth.AccessToken)
		if token == "" {
			return nil, fmt.Errorf("auth %s has no access token", authID)
		}
		key := strings.TrimSpace(f.ID)
		if key == "" {
			key = strings.TrimSpace(f.AuthIndex)
		}
		return &mmFetchTarget{
			AuthID: key,
			Realm:  accountRegion(sa),
			Token:  token,
			UID:    sa.Account.UID,
			Origin: originRefererFor(sa),
		}, nil
	}
	return nil, fmt.Errorf("auth not found: %s", authID)
}

// mmEnterprisePaths returns the candidate enterprise catalog paths. Global
// tries /v2 first (the documented 200) and keeps the legacy /console path as a
// fallback; CN only has the console path.
func mmEnterprisePaths(realm string) []string {
	if realm == realmGlobal {
		return []string{"/v2/enterprises/personal/models", "/console/enterprises/personal/models"}
	}
	return []string{"/console/enterprises/personal/models"}
}

func mmUpstreamBase(realm string) string {
	if realm == realmGlobal {
		return mmUpstreamBaseGlobal
	}
	return mmUpstreamBaseCN
}

// fetchUpstreamModels discovers the models of one account: enterprise catalog
// ∪ /v3/config, both fetched concurrently. A failing path only degrades to the
// other one; both failing is an error. Entries without an ID and entries the
// upstream marks disabled are skipped. Duplicate IDs keep the /v3/config
// fields (same rule the official client uses).
func fetchUpstreamModels(authID string) ([]upstreamModel, error) {
	target, err := mmFetchTargetFn(authID)
	if err != nil {
		return nil, err
	}
	var (
		wg            sync.WaitGroup
		ent, v3       []mmUpstreamEntry
		entIDs        map[string]bool
		entErr, v3Err error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		ent, entIDs, entErr = mmFetchCatalog(target, mmEnterprisePaths(target.Realm))
	}()
	go func() {
		defer wg.Done()
		v3, _, v3Err = mmFetchCatalog(target, []string{"/v3/config"})
	}()
	wg.Wait()
	if len(ent) == 0 && len(v3) == 0 {
		return nil, fmt.Errorf("models upstream unavailable (enterprise: %v; /v3/config: %v)", entErr, v3Err)
	}
	if entIDs == nil {
		entIDs = map[string]bool{}
	}
	// /v3/config first: on duplicate IDs its fields win.
	seen := make(map[string]bool, len(ent)+len(v3))
	out := make([]upstreamModel, 0, len(ent)+len(v3))
	for _, list := range [][]mmUpstreamEntry{v3, ent} {
		for _, e := range list {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			out = append(out, mmResolveUpstream(target.Realm, e, entIDs[e.ID]))
		}
	}
	return out, nil
}

// mmResolveUpstream maps one raw upstream entry onto the catalog shape, using
// the catalog for the Hidden flag and the static metadata tables.
func mmResolveUpstream(realm string, e mmUpstreamEntry, inEnterprise bool) upstreamModel {
	def := modelDef{
		ID:     e.ID,
		Name:   e.Name,
		Realm:  realm,
		Ctx:    e.Ctx,
		MaxTok: e.MaxOut,
		Util:   isUtilModelID(e.ID, e.MaxOut),
	}
	if cat, ok := catalogModel(realm, e.ID); ok {
		def.Hidden = cat.Hidden
		def.Tier = cat.Tier
		def.Util = def.Util || cat.Util
		if def.Name == "" {
			def.Name = cat.Name
		}
	}
	// Global: the enterprise endpoint (/v2) is the official CLI list, so a
	// model that is missing there is "hidden" (panel badge "hidden").
	if realm == realmGlobal && !inEnterprise {
		def.Hidden = true
	}
	if def.Name == "" {
		def.Name = e.ID
	}
	return upstreamModel{modelDef: def, Mult: e.Credits, Vendor: e.Vendor}
}

// mmUpstreamEntry is one parsed model of an upstream catalog response.
type mmUpstreamEntry struct {
	ID      string
	Name    string
	Credits string // "x0.14" (normalized), "" when upstream omits it
	Vendor  string
	Ctx     int64
	MaxOut  int64
}

// mmFetchCatalog GETs the first working path and parses it. The second return
// value is the set of IDs present in the response (used for the hidden flag).
func mmFetchCatalog(target *mmFetchTarget, paths []string) ([]mmUpstreamEntry, map[string]bool, error) {
	var lastErr error
	for _, p := range paths {
		entries, ids, err := mmFetchCatalogPath(target, p)
		if err != nil {
			lastErr = err
			continue
		}
		return entries, ids, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no catalog path configured")
	}
	return nil, nil, lastErr
}

func mmFetchCatalogPath(target *mmFetchTarget, path string) ([]mmUpstreamEntry, map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mmFetchTimeout)
	defer cancel()
	url := mmUpstreamBase(target.Realm) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+target.Token)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", target.Origin)
	req.Header.Set("Referer", target.Origin+"/")
	req.Header.Set("User-Agent", workBuddyUA)
	if target.UID != "" {
		req.Header.Set("X-User-Id", target.UID)
	}
	resp, err := mmModelsHTTPDo(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%s status %d", path, resp.StatusCode)
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Models []json.RawMessage `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return nil, nil, fmt.Errorf("%s decode: %w", path, err)
	}
	if payload.Code != 0 {
		return nil, nil, fmt.Errorf("%s code %d", path, payload.Code)
	}
	entries := make([]mmUpstreamEntry, 0, len(payload.Data.Models))
	ids := make(map[string]bool, len(payload.Data.Models))
	for _, raw := range payload.Data.Models {
		var m mmUpstreamModel
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		id := strings.TrimSpace(m.ID)
		if id == "" || m.Disabled {
			continue
		}
		entries = append(entries, mmUpstreamEntry{
			ID:      id,
			Name:    strings.TrimSpace(m.Name),
			Credits: parseCreditsMultiplier(m.Credits),
			Vendor:  strings.TrimSpace(m.Vendor),
			Ctx:     upstreamMaxCtx(m),
			MaxOut:  m.MaxOutputTokens,
		})
		ids[id] = true
	}
	return entries, ids, nil
}

// mmUpstreamModel is the wire shape shared by the enterprise and /v3/config
// catalogs. contextWindow is kept raw: /v3/config sends an object, older
// payloads send a plain number.
type mmUpstreamModel struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Credits         string          `json:"credits"`
	Vendor          string          `json:"vendor"`
	MaxInputTokens  int64           `json:"maxInputTokens"`
	MaxOutputTokens int64           `json:"maxOutputTokens"`
	MaxAllowedSize  int64           `json:"maxAllowedSize"`
	Disabled        bool            `json:"disabled"`
	ContextWindow   json.RawMessage `json:"contextWindow"`
}

// upstreamMaxCtx returns the maximum context window: the largest entry of
// contextWindow.supportedLengths when present (that is the real max — the
// defaultLength field is only the default), else maxInputTokens, else
// maxAllowedSize.
func upstreamMaxCtx(m mmUpstreamModel) int64 {
	if len(m.ContextWindow) > 0 {
		var cw struct {
			DefaultLength    int64   `json:"defaultLength"`
			SupportedLengths []int64 `json:"supportedLengths"`
		}
		if err := json.Unmarshal(m.ContextWindow, &cw); err == nil && len(cw.SupportedLengths) > 0 {
			max := int64(0)
			for _, l := range cw.SupportedLengths {
				if l > max {
					max = l
				}
			}
			if max > 0 {
				return max
			}
		}
		var n int64
		if err := json.Unmarshal(m.ContextWindow, &n); err == nil && n > 0 {
			return n
		}
	}
	if m.MaxInputTokens > 0 {
		return m.MaxInputTokens
	}
	return m.MaxAllowedSize
}

// parseCreditsMultiplier normalizes the upstream credits field to "x0.79".
// Upstream sends "x0.79", "x0.79 credits" or an empty string; anything
// non-numeric is dropped (panel renders "?").
func parseCreditsMultiplier(raw string) string {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return ""
	}
	v := strings.TrimPrefix(strings.ToLower(fields[0]), "x")
	if _, err := strconv.ParseFloat(v, 64); err != nil {
		return ""
	}
	return "x" + v
}

// -----------------------------------------------------------------------------
// Upstream cache
// -----------------------------------------------------------------------------

type mmFetchCacheEntry struct {
	models  []upstreamModel
	fetched time.Time
}

var mmFetchCache = struct {
	mu      sync.Mutex
	entries map[string]mmFetchCacheEntry
}{entries: map[string]mmFetchCacheEntry{}}

// upstreamModelsCached returns the upstream catalog of one account, reusing a
// result younger than mmFetchTTL. Errors are not cached.
func upstreamModelsCached(authID string) ([]upstreamModel, time.Time, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, time.Time{}, fmt.Errorf("empty auth id")
	}
	mmFetchCache.mu.Lock()
	if e, ok := mmFetchCache.entries[authID]; ok && time.Since(e.fetched) < mmFetchTTL {
		mmFetchCache.mu.Unlock()
		return append([]upstreamModel(nil), e.models...), e.fetched, nil
	}
	mmFetchCache.mu.Unlock()

	models, err := fetchUpstreamModels(authID)
	if err != nil {
		return nil, time.Time{}, err
	}
	now := time.Now()
	mmFetchCache.mu.Lock()
	mmFetchCache.entries[authID] = mmFetchCacheEntry{models: models, fetched: now}
	mmFetchCache.mu.Unlock()
	return models, now, nil
}

// invalidateUpstreamModels drops the cached catalog of one account (empty
// authID = all accounts). Used by POST /models/refresh.
func invalidateUpstreamModels(authID string) {
	mmFetchCache.mu.Lock()
	defer mmFetchCache.mu.Unlock()
	if strings.TrimSpace(authID) == "" {
		mmFetchCache.entries = map[string]mmFetchCacheEntry{}
		return
	}
	delete(mmFetchCache.entries, strings.TrimSpace(authID))
}

// -----------------------------------------------------------------------------
// State file (workbuddy-state.json)
// -----------------------------------------------------------------------------

// mmCustomModel is a user-added model (panel "custom" section).
type mmCustomModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Ctx  string `json:"ctx"`
}

// mmPrefixState is the state-file shape of the prefix toggle (contract:
// {on, val}); the panel gets {enabled, value, def} from the endpoint.
type mmPrefixState struct {
	On  bool   `json:"on"`
	Val string `json:"val"`
}

// mmFallbackState is one fallback rule (missing-model / retry-on-hard-error).
type mmFallbackState struct {
	On    bool   `json:"on"`
	Model string `json:"model"`
}

// mmSettings is one layer of settings (default / realm / per-account override).
type mmSettings struct {
	Disabled      []string        `json:"disabled"`
	Custom        []mmCustomModel `json:"custom"`
	Prefix        mmPrefixState   `json:"prefix"`
	Fallback      mmFallbackState `json:"fallback"`
	RetryFallback mmFallbackState `json:"retry_fallback"`
}

// mmUpdateState is the update-checker section owned by WORKER-1c.
type mmUpdateState struct {
	SeenVersion string `json:"seenVersion"`
}

// wbStateFile is the on-disk shape of workbuddy-state.json (frozen contract).
// The former per-account layer ("authOverride") was removed on 13 Okt; files
// that still carry it are cleaned by mmDropLegacyOverrides at configure time.
type wbStateFile struct {
	Realm  map[string]mmSettings `json:"realm"`
	Update mmUpdateState         `json:"update"`
	// DailyReport is the last growth-report run, persisted because the panel
	// shows "already reported today" per account and the in-memory summary is
	// lost on every plugin restart (which would misreport every account).
	DailyReport *persistedDailyReport `json:"daily_report,omitempty"`
	// TrialClaimed remembers accounts upstream told us already used the one-time
	// 250-credit pack (code 14051). The pack disappears from the credits list
	// once its 14-day window ends, so shape detection alone cannot tell "never
	// got it" from "got it and it expired" — this is the missing half.
	TrialClaimed map[string]bool `json:"trial_claimed,omitempty"`
}

// rememberTrialClaimed persists the fact that an account already used the
// one-time pack. Best effort: a failure only costs one extra probe later.
func rememberTrialClaimed(authIndex string) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return
	}
	_ = mutateState(func(st *wbStateFile) error {
		if st.TrialClaimed == nil {
			st.TrialClaimed = map[string]bool{}
		}
		st.TrialClaimed[authIndex] = true
		return nil
	})
}

// trialClaimedRemembered reports whether a previous claim attempt was answered
// with "already claimed".
func trialClaimedRemembered(authIndex string) bool {
	st, err := loadState()
	if err != nil || st.TrialClaimed == nil {
		return false
	}
	return st.TrialClaimed[strings.TrimSpace(authIndex)]
}

// persistedDailyReport is the on-disk form of the last daily-reward run.
type persistedDailyReport struct {
	When time.Time                    `json:"when"`
	Rows map[string]persistedDailyRow `json:"rows"` // keyed by auth_index
}

type persistedDailyRow struct {
	Day    string `json:"day,omitempty"`
	Score  int    `json:"score,omitempty"`
	Lit    bool   `json:"lit"`
	Status string `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

var (
	modelsStateMu    sync.Mutex
	modelsStateCache *wbStateFile
	modelsStatePath  string
)

// wbDataDir is the persistent plugin data dir: WORKBUDDY_DATA_DIR when set,
// else the `.codebuddy_data` subdir of the auth dir (same convention as the
// request-usage log, so the file survives plugin restarts).
func wbDataDir() string {
	base := usageLogDir()
	if strings.TrimSpace(os.Getenv("WORKBUDDY_DATA_DIR")) != "" {
		return base
	}
	return filepath.Join(base, ".codebuddy_data")
}

// wbStatePath returns the path of workbuddy-state.json.
func wbStatePath() string {
	return filepath.Join(wbDataDir(), mmStateFileName)
}

// defaultSettings returns the built-in layer: nothing disabled, no custom
// models, CN models namespaced with cn/ (Global models are bare).
func defaultSettings(realm string) mmSettings {
	on, val := false, ""
	if realm == realmCN {
		on, val = true, "cn/"
	}
	return mmSettings{
		Disabled:      []string{},
		Custom:        []mmCustomModel{},
		Prefix:        mmPrefixState{On: on, Val: val},
		Fallback:      mmFallbackState{},
		RetryFallback: mmFallbackState{},
	}
}

func emptyStateFile() *wbStateFile {
	return &wbStateFile{
		Realm: map[string]mmSettings{},
	}
}

// cloneSettings deep-copies one layer so callers cannot mutate the cache.
func cloneSettings(s mmSettings) mmSettings {
	out := s
	out.Disabled = append([]string{}, s.Disabled...)
	out.Custom = append([]mmCustomModel{}, s.Custom...)
	return out
}

// loadStateLocked reads the state file once per path and keeps it cached.
// A corrupt file is moved aside (`.corrupt`) instead of being overwritten, so
// the user's settings can still be recovered by hand.
func loadStateLocked() (*wbStateFile, error) {
	path := wbStatePath()
	if modelsStateCache != nil && modelsStatePath == path {
		return modelsStateCache, nil
	}
	state := emptyStateFile()
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var parsed wbStateFile
		if jsonErr := json.Unmarshal(raw, &parsed); jsonErr != nil {
			_ = os.Rename(path, path+".corrupt")
			state = emptyStateFile()
		} else {
			state = &parsed
			if state.Realm == nil {
				state.Realm = map[string]mmSettings{}
			}
		}
	case os.IsNotExist(err):
		// First run: defaults, nothing to persist yet.
	default:
		return nil, err
	}
	modelsStateCache = state
	modelsStatePath = path
	return state, nil
}

// loadState returns a copy of the current state file.
//
// The copy is deep for everything mutable: Realm entries are cloned, and the
// DailyReport pointer plus its Rows map and the TrialClaimed map are duplicated.
// Returning the cache's own maps/pointers would let a caller that mutates the
// result corrupt the in-memory state without writing the file, and the next
// mutateState would then persist the damage. Today every caller only reads, but
// the function says "copy" and must mean it.
func loadState() (wbStateFile, error) {
	modelsStateMu.Lock()
	defer modelsStateMu.Unlock()
	state, err := loadStateLocked()
	if err != nil {
		return wbStateFile{}, err
	}
	out := wbStateFile{
		Realm:  make(map[string]mmSettings, len(state.Realm)),
		Update: state.Update,
		// Every top-level field must be copied here. Forgetting one silently
		// hides it from all readers: DailyReport and TrialClaimed were both
		// dropped, so their persistence looked broken while the data was on
		// disk the whole time. TestLoadStateCopiesEveryField enforces this.
		DailyReport:  cloneDailyReport(state.DailyReport),
		TrialClaimed: cloneBoolMap(state.TrialClaimed),
	}
	for k, v := range state.Realm {
		out.Realm[k] = cloneSettings(v)
	}
	return out, nil
}

// cloneDailyReport duplicates the persisted run, including its Rows map, so the
// caller never shares storage with the state cache.
func cloneDailyReport(in *persistedDailyReport) *persistedDailyReport {
	if in == nil {
		return nil
	}
	out := &persistedDailyReport{When: in.When}
	if in.Rows != nil {
		out.Rows = make(map[string]persistedDailyRow, len(in.Rows))
		for k, v := range in.Rows {
			out.Rows[k] = v
		}
	}
	return out
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	if in == nil {
		return nil
	}
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// mutateState applies fn to a freshly loaded state and persists the result
// atomically. The file is re-read from disk first so a concurrent writer
// (e.g. the update checker storing seenVersion) is not clobbered.
func mutateState(fn func(*wbStateFile) error) error {
	modelsStateMu.Lock()
	defer modelsStateMu.Unlock()
	modelsStateCache = nil // force a re-read of the current file
	state, err := loadStateLocked()
	if err != nil {
		return err
	}
	if err := fn(state); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := atomicWriteFile(wbStatePath(), raw); err != nil {
		return err
	}
	return nil
}

// atomicWriteFile writes data to path via a temp file in the same directory
// (same filesystem) followed by an fsync + rename, so readers never observe a
// half-written state file.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // renamed: nothing to clean up
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// effectiveSettings resolves the settings of one realm: default → realm.
func effectiveSettings(realm string) mmSettings {
	out := defaultSettings(realm)
	state, err := loadState()
	if err != nil {
		return out
	}
	if rs, ok := state.Realm[realm]; ok {
		out = cloneSettings(rs)
	}
	return out
}

// updateRealmSettings stores the realm-wide layer (POST scope=all).
func updateRealmSettings(realm string, s mmSettings) error {
	if realm != realmGlobal && realm != realmCN {
		return fmt.Errorf("unknown realm: %s", realm)
	}
	return mutateState(func(st *wbStateFile) error {
		st.Realm[realm] = normalizeSettings(s)
		return nil
	})
}

// mmDropLegacyOverrides removes the retired per-account layer ("authOverride")
// from an existing state file by rewriting it with the current shape. Returns
// how many overrides were dropped (0 = file untouched).
func mmDropLegacyOverrides() int {
	raw, err := os.ReadFile(wbStatePath())
	if err != nil {
		return 0
	}
	var legacy struct {
		AuthOverride map[string]json.RawMessage `json:"authOverride"`
	}
	if json.Unmarshal(raw, &legacy) != nil {
		return 0
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return 0
	}
	if _, present := probe["authOverride"]; !present {
		return 0
	}
	if err := mutateState(func(*wbStateFile) error { return nil }); err != nil {
		return 0
	}
	return len(legacy.AuthOverride)
}

// resetRealmSettings drops the realm layer (GET ?reset=1&realm=R).
func resetRealmSettings(realm string) error {
	if realm != realmGlobal && realm != realmCN {
		return fmt.Errorf("unknown realm: %s", realm)
	}
	return mutateState(func(st *wbStateFile) error {
		delete(st.Realm, realm)
		return nil
	})
}

// normalizeSettings makes a settings layer safe to store: nil slices become
// empty (so the JSON keeps "disabled": [] instead of null) and the prefix
// value is trimmed.
func normalizeSettings(s mmSettings) mmSettings {
	if s.Disabled == nil {
		s.Disabled = []string{}
	}
	if s.Custom == nil {
		s.Custom = []mmCustomModel{}
	}
	s.Prefix.Val = strings.TrimSpace(s.Prefix.Val)
	s.Fallback.Model = strings.TrimSpace(s.Fallback.Model)
	s.RetryFallback.Model = strings.TrimSpace(s.RetryFallback.Model)
	if !s.Prefix.On {
		// The panel clears the value when the toggle is off; keep it empty so
		// "off" never leaks a stale prefix into routing.
		s.Prefix.Val = ""
	}
	return s
}

// loadUpdateState returns the update section (WORKER-1c integration point).
func loadUpdateState() mmUpdateState {
	state, err := loadState()
	if err != nil {
		return mmUpdateState{}
	}
	return state.Update
}

// saveUpdateSeenVersion stores the version the user chose to skip.
func saveUpdateSeenVersion(v string) error {
	return mutateState(func(st *wbStateFile) error {
		st.Update.SeenVersion = strings.TrimSpace(v)
		return nil
	})
}

// -----------------------------------------------------------------------------
// /models/config payload
// -----------------------------------------------------------------------------

// mmModelRow is one model row of GET /models/config (panel contract).
type mmModelRow struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Ctx          string `json:"ctx,omitempty"`
	Mult         string `json:"mult,omitempty"`
	Hidden       bool   `json:"hidden,omitempty"`
	Verified     bool   `json:"verified,omitempty"`
	Stale        bool   `json:"stale,omitempty"`
	Effort       bool   `json:"effort,omitempty"`
	Tier         bool   `json:"tier,omitempty"`
	Util         bool   `json:"util,omitempty"`
	Custom       bool   `json:"custom,omitempty"`
	OwnedBy      string `json:"owned_by,omitempty"`
	AllExhausted bool   `json:"all_exhausted,omitempty"`
	New          bool   `json:"new,omitempty"`
}

// accountsForRealm lists the AuthIDs of one realm. Seam for tests: the host
// auth RPC is unavailable outside a running CPA host.
var mmAccountsForRealmFn = accountsForRealm
var mmAccountLabelsForRealmFn = accountLabelsForRealm

// Account values stay host record IDs. Labels are display-only, derived from
// stored nicknames on every refresh, including newly imported credentials.
func accountLabelsForRealm(realm string) map[string]string {
	labels := map[string]string{}
	files, err := authListSource()
	if err != nil {
		return labels
	}
	for _, f := range files {
		sa, err := authGetSource(f.AuthIndex)
		if err != nil || sa == nil || accountRegion(sa) != realm {
			continue
		}
		id := firstNonEmpty(strings.TrimSpace(f.ID), strings.TrimSpace(f.AuthIndex))
		if id == "" {
			continue
		}
		label := strings.TrimSpace(sa.Account.Nickname)
		if label == "" {
			label = strings.TrimSpace(f.Label)
			for _, suffix := range []string{" [Global]", " [CN]"} {
				label = strings.TrimSuffix(label, suffix)
			}
		}
		labels[id] = firstNonEmpty(label, id)
	}
	return labels
}

// accountsForRealm lists the AuthIDs of one realm.
func accountsForRealm(realm string) []string {
	files, err := hostAuthList()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		sa, _, err := hostAuthGetBundle(f.AuthIndex)
		if err != nil {
			continue
		}
		if accountRegion(sa) != realm {
			continue
		}
		key := strings.TrimSpace(f.ID)
		if key == "" {
			key = strings.TrimSpace(f.AuthIndex)
		}
		if key != "" {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// modelsConfigPayload builds the whole GET /models/config response. fetchAccount
// selects which account of the realm is used for the upstream fetch (empty =
// first account of the realm).
func modelsConfigPayload(fetchAccounts map[string]string) map[string]any {
	realms := map[string]any{
		realmGlobal: modelsRealmPayload(realmGlobal, fetchAccounts[realmGlobal]),
		realmCN:     modelsRealmPayload(realmCN, fetchAccounts[realmCN]),
	}
	return map[string]any{
		"realms":  realms,
		"version": map[string]any{"current": version},
	}
}

func modelsRealmPayload(realm, fetchAccount string) map[string]any {
	accounts := mmAccountsForRealmFn(realm)
	fetchAccount = strings.TrimSpace(fetchAccount)
	if fetchAccount == "" || !containsString(accounts, fetchAccount) {
		if len(accounts) > 0 {
			fetchAccount = accounts[0]
		}
	}
	settings := effectiveSettings(realm)

	var (
		upstream  []upstreamModel
		fetchedAt string
		upErr     string
	)
	if fetchAccount != "" {
		models, at, err := upstreamModelsCached(fetchAccount)
		if err != nil {
			upErr = err.Error()
		} else {
			upstream = models
			fetchedAt = at.UTC().Format(time.RFC3339)
		}
	}

	rows := buildModelRows(realm, upstream, settings.Custom)
	catalogCount, freeCount := 0, 0
	for _, r := range rows {
		if r.Stale {
			catalogCount++
		}
		if r.Mult == "x0.00" {
			freeCount++
		}
	}
	prefixOn, prefixVal := settings.Prefix.On, settings.Prefix.Val
	out := map[string]any{
		"accounts":       orEmptyStrings(accounts),
		"account_labels": mmAccountLabelsForRealmFn(realm),
		"fetch_account":  fetchAccount,
		"fetched_at":     fetchedAt,
		"upstream":       len(upstream),
		"catalog":        catalogCount,
		"free":           freeCount,
		"prefix":         map[string]any{"enabled": prefixOn, "value": prefixVal, "def": mmPrefixPlaceholder(realm)},
		"disabled":       orEmptyStrings(settings.Disabled),
		"custom":         customRows(settings.Custom),
		"fallback":       map[string]any{"enabled": settings.Fallback.On, "model": settings.Fallback.Model},
		"retry_fallback": map[string]any{"enabled": settings.RetryFallback.On, "model": settings.RetryFallback.Model},
		"models":         rows,
	}
	if upErr != "" {
		out["fetch_error"] = upErr
	}
	return out
}

// mmPrefixPlaceholder is the prefix shown in the input when the toggle is off.
func mmPrefixPlaceholder(realm string) string {
	if realm == realmCN {
		return "cn/"
	}
	return "cb/"
}

func customRows(custom []mmCustomModel) []map[string]any {
	out := make([]map[string]any, 0, len(custom))
	for _, c := range custom {
		out = append(out, map[string]any{"id": c.ID, "name": c.Name, "ctx": c.Ctx})
	}
	return out
}

// buildModelRows merges catalog ∪ upstream ∪ custom into the panel row list.
// Upstream rows carry the live multiplier and the max context; catalog entries
// the upstream no longer returns are marked stale (archived multiplier when
// known). Rows are ordered the way the panel renders them: tier, upstream
// (per vendor, cheapest first), verified, utility, stale, custom.
func buildModelRows(realm string, upstream []upstreamModel, custom []mmCustomModel) []mmModelRow {
	upstreamIDs := make(map[string]bool, len(upstream))
	rows := make([]mmModelRow, 0, len(upstream)+len(catalogModels(realm)))
	for _, u := range upstream {
		upstreamIDs[u.ID] = true
		rows = append(rows, mmModelRow{
			ID:       u.ID,
			Name:     u.Name,
			Ctx:      formatCtx(u.Ctx),
			Mult:     u.Mult,
			Hidden:   u.Hidden,
			Verified: verifiedModelIDs[realm+"/"+u.ID],
			Effort:   effortModelIDs[realm+"/"+u.ID],
			Tier:     u.Tier,
			Util:     u.Util,
			New:      !catalogHas(realm, u.ID),
		})
	}
	for _, c := range catalogModels(realm) {
		if c.ID == "" || upstreamIDs[c.ID] {
			continue
		}
		rows = append(rows, mmModelRow{
			ID:       c.ID,
			Name:     c.Name,
			Ctx:      formatCtx(c.Ctx),
			Mult:     archivedMultipliers[realm+"/"+c.ID],
			Hidden:   c.Hidden,
			Verified: verifiedModelIDs[realm+"/"+c.ID],
			Effort:   effortModelIDs[realm+"/"+c.ID],
			Tier:     c.Tier,
			Util:     c.Util,
			Stale:    true,
		})
	}
	for _, c := range custom {
		if c.ID == "" {
			continue
		}
		rows = append(rows, mmModelRow{ID: c.ID, Name: c.Name, Ctx: c.Ctx, Mult: "?", Custom: true})
	}
	sortModelRows(rows)
	return rows
}

func catalogHas(realm, id string) bool {
	_, ok := catalogModel(realm, id)
	return ok
}

// sortModelRows orders the rows the way the panel renders them (the panel only
// inserts the vendor sub-headers when the family changes, so upstream rows must
// already be grouped per vendor and sorted by price inside a vendor).
func sortModelRows(rows []mmModelRow) {
	rank := func(r mmModelRow) int {
		switch {
		case r.Custom:
			return 5
		case r.Stale:
			return 4
		case r.Util:
			return 3
		case r.Verified:
			return 2
		case r.Tier:
			return 0
		default:
			return 1
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := rank(rows[i]), rank(rows[j])
		if ri != rj {
			return ri < rj
		}
		if ri != 1 {
			return false
		}
		fi, fj := modelVendorFamily(rows[i].ID), modelVendorFamily(rows[j].ID)
		if fi != fj {
			if fi == "" {
				return false
			}
			if fj == "" {
				return true
			}
			return fi < fj
		}
		return rowPrice(rows[i]) < rowPrice(rows[j])
	})
}

// rowPrice parses the multiplier for ordering; unknown prices sort last.
func rowPrice(r mmModelRow) float64 {
	v := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(r.Mult)), "x")
	if v == "" {
		return 1 << 30
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 1 << 30
	}
	return f
}

// modelVendorFamily mirrors the panel's mmFamilyOf() grouping (vendor
// sub-headers in the upstream section).
func modelVendorFamily(id string) string {
	lower := strings.ToLower(id)
	switch {
	case strings.HasPrefix(lower, "deepseek"):
		return "DeepSeek"
	case strings.HasPrefix(lower, "gemini"):
		return "Gemini"
	case strings.HasPrefix(lower, "glm"):
		return "GLM (Zhipu)"
	case strings.HasPrefix(lower, "gpt"):
		return "GPT ([OI])"
	case strings.HasPrefix(lower, "grok"):
		return "Grok (xAI)"
	case strings.HasPrefix(lower, "hy"):
		return "Hy (Tencent)"
	case strings.HasPrefix(lower, "kimi"):
		return "Kimi (Moonshot)"
	case strings.HasPrefix(lower, "minimax"):
		return "MiniMax"
	case strings.HasPrefix(lower, "step"):
		return "StepFun"
	case strings.HasPrefix(lower, "space"):
		return "Space-Bunny"
	case strings.HasPrefix(lower, "hunyuan"):
		return "Hunyuan (Tencent)"
	}
	return ""
}

// formatCtx renders a token count the way the panel shows it (1M, 500k, 176k).
// Values are rounded to the nearest 1k so odd catalog limits (131072, 262144)
// do not show up as raw numbers.
func formatCtx(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n >= 1000000 && n%1000000 == 0:
		return strconv.FormatInt(n/1000000, 10) + "M"
	case n >= 1000000:
		return strconv.FormatFloat(float64(n)/1000000, 'f', 1, 64) + "M"
	default:
		return strconv.FormatInt((n+500)/1000, 10) + "k"
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func orEmptyStrings(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// -----------------------------------------------------------------------------
// Management endpoint: GET/POST /models/config
// -----------------------------------------------------------------------------

// handleModelsConfigGet serves GET /models/config:
//
//	?realm=global|cn&fetch_account=<authId>   catalog + effective state
//	?reset=1&realm=R                          reset the realm layer to default
//
// The whole payload (both realms) is always returned — the panel renders both
// popups from one call.
func handleModelsConfigGet(req pluginapi.ManagementRequest) map[string]any {
	query := req.Query
	fetch := map[string]string{}
	for _, realm := range []string{realmGlobal, realmCN} {
		if vals := query["fetch_account_"+realm]; len(vals) > 0 {
			fetch[realm] = strings.TrimSpace(vals[0])
		}
	}
	if vals := query["realm"]; len(vals) > 0 {
		realm := strings.ToLower(strings.TrimSpace(vals[0]))
		if realm == realmGlobal || realm == realmCN {
			if vals := query["fetch_account"]; len(vals) > 0 {
				fetch[realm] = strings.TrimSpace(vals[0])
			}
		}
	}
	if vals := query["reset"]; len(vals) > 0 && truthyQuery(vals[0]) {
		realm := ""
		if r := query["realm"]; len(r) > 0 {
			realm = strings.ToLower(strings.TrimSpace(r[0]))
		}
		if realm != realmGlobal && realm != realmCN {
			return map[string]any{"error": "reset requires realm=global|cn"}
		}
		if err := resetRealmSettings(realm); err != nil {
			return map[string]any{"error": err.Error()}
		}
		payload := modelsConfigPayload(fetch)
		payload["reload"] = mmTriggerHostReload(req.Headers)
		return payload
	}
	return modelsConfigPayload(fetch)
}

// handleModelsConfigPost serves POST /models/config:
//
//	{realm, scope:"all"|"acct"|"auth", auth, state:{...}, remove?:bool}
//
// scope "all" writes the realm layer, "acct"/"auth" writes (or with remove:true
// deletes) the per-account override.
func handleModelsConfigPost(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Realm string `json:"realm"`
		Scope string `json:"scope"`
		Auth  string `json:"auth"`
		State struct {
			Disabled           []string        `json:"disabled"`
			Custom             []mmCustomModel `json:"custom"`
			PrefixEnabled      bool            `json:"prefix_enabled"`
			PrefixValue        string          `json:"prefix_value"`
			FallbackEnabled    bool            `json:"fallback_enabled"`
			FallbackModel      string          `json:"fallback_model"`
			RetryFallbackOn    bool            `json:"retry_fallback_enabled"`
			RetryFallbackModel string          `json:"retry_fallback_model"`
		} `json:"state"`
	}
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return map[string]any{"error": "invalid body: " + err.Error()}
		}
	}
	realm := strings.ToLower(strings.TrimSpace(body.Realm))
	if realm != realmGlobal && realm != realmCN {
		return map[string]any{"error": "realm must be global or cn"}
	}
	settings := mmSettings{
		Disabled:      body.State.Disabled,
		Custom:        body.State.Custom,
		Prefix:        mmPrefixState{On: body.State.PrefixEnabled, Val: body.State.PrefixValue},
		Fallback:      mmFallbackState{On: body.State.FallbackEnabled, Model: body.State.FallbackModel},
		RetryFallback: mmFallbackState{On: body.State.RetryFallbackOn, Model: body.State.RetryFallbackModel},
	}
	if err := validatePrefixSettings(realm, settings); err != nil {
		return map[string]any{"error": err.Error()}
	}
	switch strings.ToLower(strings.TrimSpace(body.Scope)) {
	case "", "all":
	default:
		// The per-account layer was removed (13 Okt): an old panel tab that
		// still sends scope=acct/auth gets a clear error instead of a silent
		// realm-wide write.
		return map[string]any{"error": "per-account settings were removed; save for all accounts of the realm"}
	}
	if err := updateRealmSettings(realm, settings); err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Trigger a host model-registry reload so a prefix/disabled change is
	// visible on /v1/models immediately (CPA 7.2.145 PATCH plugin-config hook).
	// A reload failure never fails the save: the state is already persisted.
	reload := mmTriggerHostReload(req.Headers)
	if reload.Status != "ok" {
		guardLogf("model-manager: live reload %s (http=%d): %s", reload.Status, reload.HTTP, reload.Error)
	}
	// The panel refreshes with a GET right after saving; return the fresh
	// payload anyway so a failed refresh still shows the saved state.
	payload := modelsConfigPayload(map[string]string{realm: strings.TrimSpace(body.Auth)})
	payload["reload"] = reload
	return payload
}

// modelsConfigResetRequest reports whether the request is the GET ?reset=1
// mutation of /models/config. It writes the state file, so it gets the same
// guard as a POST (management key + rate limit).
func modelsConfigResetRequest(req pluginapi.ManagementRequest) bool {
	if req.Method != http.MethodGet {
		return false
	}
	if !strings.HasSuffix(strings.TrimRight(req.Path, "/"), "/models/config") {
		return false
	}
	vals := req.Query["reset"]
	return len(vals) > 0 && truthyQuery(vals[0])
}

// truthyQuery accepts the flag shapes the panel and humans send.
func truthyQuery(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
