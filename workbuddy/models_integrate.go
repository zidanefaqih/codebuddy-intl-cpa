// models_integrate.go wires the Model Manager endpoints that need cross-module
// access (W1d): the upstream refresh fan-out, the per-model live test and the
// legacy models stub removal glue. It exists as a separate file so
// models_config.go (W1a) and update_check.go (W1c) stay untouched; everything
// here is glue over the pieces W1a/W1c delivered.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// mmChatHTTPDo is the HTTP seam for the model test request (tests swap it).
var mmChatHTTPDo = hostHTTPDo

// handleModelsRefresh serves POST /models/refresh:
//
//	{realm: "global"|"cn"} — fan-out: drop the cached upstream catalog of every
//	account in the realm, then re-fetch (cheap console GETs, no chat credits).
//	Returns the refreshed payload for the realm so the panel can render
//	immediately without a second round-trip.
func handleModelsRefresh(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Realm string `json:"realm"`
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
	accounts := mmAccountsForRealmFn(realm)
	if len(accounts) == 0 {
		return map[string]any{"error": "no " + realm + " accounts found"}
	}
	refreshed, failures := 0, 0
	var lastErr string
	for _, authID := range accounts {
		invalidateUpstreamModels(authID)
		if _, _, err := upstreamModelsCached(authID); err != nil {
			failures++
			lastErr = err.Error()
			continue
		}
		refreshed++
	}
	if failures > 0 && refreshed == 0 {
		return map[string]any{"error": "refresh failed for all accounts: " + lastErr}
	}
	out := modelsConfigPayload(map[string]string{realm: accounts[0]})
	out["refresh"] = map[string]any{"refreshed": refreshed, "failed": failures, "total": len(accounts)}
	if lastErr != "" {
		out["refresh"].(map[string]any)["last_error"] = lastErr
	}
	return out
}

// handleModelsTest serves POST /models/test:
//
//	{realm, auth, model} — one tiny real chat completion against the upstream
//	using the selected account. Returns status, served model, token usage and
//	credit so the panel's Test column shows ground truth, not catalog claims.
//	Cost guard: max_tokens 32 and a trivial prompt — verified credit cost of
//	this shape is 0.00–0.04 on the cheapest models, bounded by max_tokens.
func handleModelsTest(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Realm   string `json:"realm"`
		Auth    string `json:"auth"`
		Account string `json:"account"` // alias used by the panel (W2)
		Model   string `json:"model"`
	}
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return map[string]any{"error": "invalid body: " + err.Error()}
		}
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		return map[string]any{"error": "model is required"}
	}
	authID := strings.TrimSpace(body.Auth)
	if authID == "" {
		authID = strings.TrimSpace(body.Account)
	}
	if authID == "" {
		return map[string]any{"error": "auth is required"}
	}
	target, err := resolveFetchTarget(authID)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	payload := map[string]any{
		"model":      model,
		"stream":     true, // upstream rejects non-stream (code 11101)
		"messages":   []map[string]any{{"role": "system", "content": "You are a helpful assistant."}, {"role": "user", "content": "reply with exactly: OK"}},
		"max_tokens": 32,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	base := mmUpstreamBaseGlobal
	if target.Realm == realmCN {
		base = mmUpstreamBaseCN
	}
	ctxDone := make(chan struct{})
	defer close(ctxDone)
	httpReq, err := http.NewRequest(http.MethodPost, base+"/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	httpReq.Header.Set("Authorization", "Bearer "+target.Token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Origin", target.Origin)
	httpReq.Header.Set("Referer", target.Origin+"/")
	httpReq.Header.Set("User-Agent", workBuddyUA)
	if target.UID != "" {
		httpReq.Header.Set("X-User-Id", target.UID)
	}
	resp, err := mmChatHTTPDo(httpReq)
	if err != nil {
		return map[string]any{"ok": false, "status": 0, "error": "upstream request failed: " + err.Error()}
	}

	out := map[string]any{"ok": resp.StatusCode == http.StatusOK, "status": resp.StatusCode, "http_status": resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		// Upstream error envelope: {"code":11102,"msg":"..."} — surface code+msg
		// so the panel can explain retired models (11102) vs effort (11150).
		var apiErr struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(resp.Body, &apiErr); err == nil && apiErr.Code != 0 {
			out["upstream_code"] = apiErr.Code
			out["error"] = fmt.Sprintf("upstream %d: code %d %s", resp.StatusCode, apiErr.Code, strings.TrimSpace(apiErr.Msg))
		} else {
			out["error"] = fmt.Sprintf("upstream status %d", resp.StatusCode)
		}
		return out
	}

	// Fold the SSE stream manually: last usage chunk carries credit + served model.
	var content, served string
	var usage map[string]any
	for _, line := range strings.Split(string(resp.Body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
			continue
		}
		if chunk.Model != "" {
			served = chunk.Model
		}
		for _, c := range chunk.Choices {
			content += c.Delta.Content
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	out["served"] = served
	out["content"] = truncateTestContent(content, 40)
	if usage != nil {
		if v, ok := usage["total_tokens"].(float64); ok {
			out["total_tokens"] = int64(v)
		}
		if v, ok := usage["credit"].(float64); ok {
			out["credit"] = v
		}
	}
	if served != "" && served != model {
		out["redirected"] = true
	}
	return out
}

func truncateTestContent(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- Dynamic prefix emission + strip (Model Manager decision 4) ----
//
// The Model Manager prefix settings replace the hardcoded cn/ behaviour:
// effectiveSettings(realm).Prefix{On,Val} decides whether the realm's models
// are emitted with a prefix and which one (empty value + On = realm default:
// cn/ for CN, cb/ for Global). Global default is OFF (bare IDs, the historical
// behaviour), CN default is ON cn/. Settings are per realm only — the
// per-account override layer was removed (PLAN-0907b follow-up, Noel 13 Okt).

// mmPrefixFromSettings resolves the client-facing prefix of one settings layer
// ("" = bare). On with an empty value = realm default (cn/ for CN, cb/ for
// Global).
func mmPrefixFromSettings(realm string, s mmSettings) string {
	if !s.Prefix.On {
		return ""
	}
	v := strings.TrimSpace(s.Prefix.Val)
	if v == "" {
		return mmDefaultPrefix(realm)
	}
	if !strings.HasSuffix(v, "/") {
		v += "/"
	}
	return v
}

// mmEffectivePrefix returns the prefix string to emit for a realm ("" = bare).
func mmEffectivePrefix(realm string) string {
	return mmPrefixFromSettings(realm, effectiveSettings(realm))
}

// mmDefaultPrefix is the per-realm default prefix (used when On && Val == "").
func mmDefaultPrefix(realm string) string {
	if realm == realmCN {
		return "cn/"
	}
	return "cb/"
}

// mmOtherRealm returns the realm opposite to realm.
func mmOtherRealm(realm string) string {
	if realm == realmCN {
		return realmGlobal
	}
	return realmCN
}

// mmPrefixValuePattern accepts the prefix characters the panel may send:
// A-Za-z0-9._- with an optional trailing slash. An empty value means "use the
// realm default".
var mmPrefixValuePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/?$`)

// validatePrefixSettings rejects a Save whose effective prefix would collide
// with the other realm or with a reserved legacy prefix (PLAN-0907b T2).
func validatePrefixSettings(realm string, s mmSettings) error {
	if !s.Prefix.On {
		return nil
	}
	raw := strings.TrimSpace(s.Prefix.Val)
	if raw != "" && !mmPrefixValuePattern.MatchString(raw) {
		return fmt.Errorf("invalid prefix %s: use only A-Za-z0-9._- followed by an optional /", raw)
	}
	own := mmPrefixFromSettings(realm, s)
	if own == "" {
		return nil
	}
	other := mmOtherRealm(realm)
	if otherPfx := mmEffectivePrefix(other); otherPfx != "" && strings.EqualFold(own, otherPfx) {
		return fmt.Errorf("prefix %s collides with the %s realm prefix %s", own, realmLabel(other), otherPfx)
	}
	if realm == realmGlobal && strings.EqualFold(own, "cn/") {
		return fmt.Errorf("prefix %s is reserved for CN models", own)
	}
	if realm == realmCN && strings.EqualFold(own, "cb/") {
		return fmt.Errorf("prefix %s is reserved for Global models", own)
	}
	return nil
}

// mmPrefixRealm reports which realm owns a client-facing model prefix
// ("" = bare / unknown). Used for realm-specific failover pools and for
// isCNModelID. Detection uses the realm-layer effective prefixes plus the
// legacy cn/ and cb/ defaults.
func mmPrefixRealm(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}
	if strings.HasPrefix(m, "cn/") {
		return realmCN
	}
	if strings.HasPrefix(m, "cb/") {
		return realmGlobal
	}
	// Check the realm prefixes from longest to shortest so a shorter prefix
	// cannot shadow a longer one.
	type rp struct {
		realm  string
		prefix string
	}
	candidates := []rp{
		{realmCN, mmEffectivePrefix(realmCN)},
		{realmGlobal, mmEffectivePrefix(realmGlobal)},
	}
	best := ""
	bestRealm := ""
	for _, c := range candidates {
		if c.prefix == "" {
			continue
		}
		p := strings.ToLower(c.prefix)
		if strings.HasPrefix(m, p) && len(p) > len(best) {
			best = p
			bestRealm = c.realm
		}
	}
	return bestRealm
}

// applyDynamicPrefix prefixes/normalizes the emitted model IDs for one realm.
// Replaces prefixCNModels (hardcoded cn/).
func applyDynamicPrefix(models []pluginapi.ModelInfo, realm string) []pluginapi.ModelInfo {
	pfx := mmEffectivePrefix(realm)
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if pfx == "" {
			// bare: strip a leading default prefix if the id carries one
			low := strings.ToLower(m.ID)
			if strings.HasPrefix(low, "cn/") {
				m.ID = strings.TrimSpace(m.ID[3:])
			} else if strings.HasPrefix(low, "cb/") {
				m.ID = strings.TrimSpace(m.ID[3:])
			}
		} else {
			if !strings.HasPrefix(strings.ToLower(m.ID), strings.ToLower(pfx)) {
				m.ID = pfx + m.ID
			}
		}
		out = append(out, m)
	}
	return out
}

// ---- Host model-registry live reload (PLAN-0907b T1) ----

// mmReloadResult is the outcome of one host reload trigger. It is surfaced in
// the Save response as {"reload": {...}} so the panel can tell the user whether
// /v1/models already reflects the change or a CPA restart is still needed.
type mmReloadResult struct {
	Status string `json:"status"` // ok | failed | disabled | no_credentials
	HTTP   int    `json:"http,omitempty"`
	Error  string `json:"error,omitempty"`
}

// mmReloadClient deliberately bypasses the host HTTP bridge and the host's
// proxy-url: the target is the CPA management API on localhost, and a proxy
// would either swallow the call or route it through an upstream proxy.
var mmReloadClient = &http.Client{
	Timeout: 3 * time.Second,
	Transport: &http.Transport{
		Proxy:              nil,
		MaxIdleConns:       2,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
	},
}

// mmReloadHTTPDo is the HTTP seam for the reload trigger (tests swap it).
var mmReloadHTTPDo = func(req *http.Request) (*http.Response, error) {
	return mmReloadClient.Do(req)
}

var (
	mmReloadRevMu sync.Mutex
	mmReloadRev   int64
)

// mmNextModelsRev returns a strictly increasing revision. The host skips a
// reload when the value equals the previous one ("hash match"), so two saves
// inside the same nanosecond must still differ.
func mmNextModelsRev() string {
	now := time.Now().UnixNano()
	mmReloadRevMu.Lock()
	if now <= mmReloadRev {
		now = mmReloadRev + 1
	}
	mmReloadRev = now
	mmReloadRevMu.Unlock()
	return strconv.FormatInt(now, 10)
}

// mmTriggerHostReload asks the host to refresh the plugin model registry after
// a successful settings save, by PATCHing one always-changing field
// (models_rev) into the plugin's own config. CPA 7.2.145 answers that route
// with an explicit config-reload hook (reloadConfigAfterManagementSaveAsync →
// ReloadConfigIfChanged → syncPluginModelRuntime), so /v1/models goes fresh in
// a few hundred ms without restarting CPA.
//
// The management credential is never stored: only the Authorization /
// X-Management-Key headers of the original Save request are forwarded, and the
// value never reaches a log line.
func mmTriggerHostReload(hdr http.Header) mmReloadResult {
	if !liveReloadEnabled() {
		return mmReloadResult{Status: "disabled"}
	}
	authz := strings.TrimSpace(hdr.Get("Authorization"))
	mgmtKey := strings.TrimSpace(hdr.Get("X-Management-Key"))
	if authz == "" && mgmtKey == "" {
		return mmReloadResult{Status: "no_credentials"}
	}
	url := strings.TrimRight(selfURLValue(), "/") + "/v0/management/plugins/" + pluginRouteID + "/config"
	body := []byte(`{"models_rev":"` + mmNextModelsRev() + `"}`)
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return mmReloadResult{Status: "failed", Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if mgmtKey != "" {
		req.Header.Set("X-Management-Key", mgmtKey)
	}
	resp, err := mmReloadHTTPDo(req)
	if err != nil {
		return mmReloadResult{Status: "failed", Error: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mmReloadResult{Status: "failed", HTTP: resp.StatusCode, Error: fmt.Sprintf("management API returned %d", resp.StatusCode)}
	}
	return mmReloadResult{Status: "ok", HTTP: resp.StatusCode}
}

// mmFilterDisabled removes models the user turned off in the Model Manager
// (realm layer). Strips any known
// prefix before the lookup because the disabled list stores bare catalog ids.
func mmFilterDisabled(models []pluginapi.ModelInfo, realm string) []pluginapi.ModelInfo {
	st := effectiveSettings(realm)
	if len(st.Disabled) == 0 {
		return models
	}
	off := make(map[string]struct{}, len(st.Disabled))
	for _, id := range st.Disabled {
		off[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}
	prefixes := mmKnownPrefixes(realm)
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		id := strings.ToLower(strings.TrimSpace(m.ID))
		// compare bare and prefixed forms so emission prefix never hides the rule
		if _, hit := off[id]; hit {
			continue
		}
		strip := ""
		for _, pfx := range prefixes {
			if strings.HasPrefix(id, pfx) {
				strip = pfx
				break
			}
		}
		if strip != "" {
			if _, hit2 := off[strings.TrimPrefix(id, strip)]; hit2 {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// mmEmissionUpstreamFn is the test seam for the per-account upstream catalog
// used by /v1/models emission.
var mmEmissionUpstreamFn = upstreamModelsForEmission

// upstreamModelsForEmission returns one account's upstream catalog (the same
// /v2 ∪ /v3/config fetch + 5 min cache the panel uses). When a refresh fails it
// keeps serving the last good result of any age, so a flaky upstream never
// shrinks /v1/models; nil when the account was never fetched successfully.
func upstreamModelsForEmission(authID string) []upstreamModel {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	if models, _, err := upstreamModelsCached(authID); err == nil {
		return models
	}
	mmFetchCache.mu.Lock()
	defer mmFetchCache.mu.Unlock()
	if e, ok := mmFetchCache.entries[authID]; ok {
		return append([]upstreamModel(nil), e.models...)
	}
	return nil
}

// mmEmissionModels builds the client-facing model list of one account as the
// union the Model Manager panel shows (PLAN-0907 decision 2, CB-039):
// base (dynamic discovery / static fallback) ∪ catalog ∪ upstream ∪ custom.
// Utility models (no public chat billing) stay out — the panel labels them
// "default off"; a user who wants one can add it as a custom model. Earlier
// entries win on duplicate IDs (case-insensitive).
func mmEmissionModels(base []pluginapi.ModelInfo, realm, authID string) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(base)+48)
	seen := make(map[string]bool, len(base)+48)
	add := func(m pluginapi.ModelInfo) {
		key := strings.ToLower(strings.TrimSpace(m.ID))
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, m)
	}
	for _, m := range base {
		add(m)
	}
	for _, m := range catalogModelInfos(realm) {
		add(m)
	}
	for _, u := range mmEmissionUpstreamFn(authID) {
		if u.Util {
			continue
		}
		add(modelDefInfo(u.modelDef))
	}
	for _, c := range effectiveSettings(realm).Custom {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = c.ID
		}
		add(pluginapi.ModelInfo{ID: strings.TrimSpace(c.ID), Name: name, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}})
	}
	return out
}

// mmKnownPrefixes lists every prefix the plugin can emit for a realm: its
// effective prefix, the other realm's effective prefix and
// the two legacy defaults (cn/ cb/). Used to strip before disabled lookups and
// to keep old client configs working.
func mmKnownPrefixes(realm string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	add := func(p string) {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	add(mmEffectivePrefix(realm))
	add(mmEffectivePrefix(mmOtherRealm(realm)))
	add("cn/")
	add("cb/")
	return out
}

// mmRealmForAuth resolves the realm of the auth behind a storage JSON.
func mmRealmForAuth(storageJSON []byte) string {
	if sa, err := parseStored(storageJSON); err == nil {
		return accountRegion(sa)
	}
	if authUsesGlobalModels(storageJSON) {
		return realmGlobal
	}
	return realmCN
}

// stripDynamicPrefix removes whatever prefix the realm currently emits from a
// requested model id (client-facing alias → upstream id). Falls back to the
// other realm's effective prefix and the hardcoded cn/ cb/ defaults so old
// client configs (cn/glm-5.3) keep resolving after a prefix change.
func stripDynamicPrefix(model, realm string) (string, bool) {
	m := strings.TrimSpace(model)
	low := strings.ToLower(m)
	own := mmEffectivePrefix(realm)
	if own != "" && strings.HasPrefix(low, strings.ToLower(own)) {
		return strings.TrimSpace(m[len(own):]), true
	}
	for _, pfx := range mmKnownPrefixes(realm) {
		if pfx == strings.ToLower(own) {
			continue
		}
		if strings.HasPrefix(low, pfx) {
			return strings.TrimSpace(m[len(pfx):]), true
		}
	}
	return m, false
}

// mmRealmHintFromAttributes guesses the realm for a request from its routing
// attributes (fallback only, when the executing auth is unknown).
func mmRealmHintFromAttributes(attributes map[string]string) string {
	for _, v := range attributes {
		lv := strings.ToLower(v)
		if strings.Contains(lv, "global") || strings.Contains(lv, "intl") || strings.Contains(lv, "cb/") {
			return realmGlobal
		}
		if strings.Contains(lv, "cn/") {
			return realmCN
		}
	}
	return realmCN
}
