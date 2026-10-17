// credits_handler.go implements the management API endpoints that mutate or
// read account state: import credential, toggle check-in, claim trial, select
// active auth, and query credits for one account or all.
package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// handleImportAuth accepts nested or flat credential JSON and persists via host.auth.save.
func handleImportAuth(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		JSON json.RawMessage `json:"json"`
		Raw  string          `json:"raw"`
	}
	_ = json.Unmarshal(req.Body, &body)
	raw := []byte(strings.TrimSpace(body.Raw))
	if len(body.JSON) > 0 {
		raw = body.JSON
	}
	if len(raw) == 0 {
		return map[string]any{"success": false, "error": "missing json/raw credential payload"}
	}
	sa, err := parseStored(raw)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	// A re-import can race with refresh/lifecycle writes and may target an
	// existing legacy workbuddy.json. Resolve every visible identity, lock it,
	// then take a fresh physical snapshot before saving.
	auth := toAuthData(sa)
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"success": false, "error": "host.auth.list: " + err.Error()}
	}
	var existingEntry *pluginapi.HostAuthFileEntry
	identities := []string{sa.Account.UID, auth.FileName}
	for i := range files {
		f := &files[i]
		matched := strings.EqualFold(strings.TrimSpace(f.Name), auth.FileName) ||
			strings.EqualFold(strings.TrimSpace(f.ID), auth.FileName) ||
			(sa.Account.UID != "" && listEntryMatchesUID(*f, sa.Account.UID, auth.FileName))
		if !matched && sa.Account.UID != "" && f.AuthIndex != "" {
			if current, getErr := hostAuthGet(f.AuthIndex); getErr == nil &&
				strings.EqualFold(strings.TrimSpace(current.Account.UID), strings.TrimSpace(sa.Account.UID)) {
				matched = true
			}
		}
		if matched {
			existingEntry = f
			identities = append(identities, f.AuthIndex, f.ID, f.Name)
			break
		}
	}
	unlock := lockAuthMutationKeys(identities...)
	defer unlock()

	var existing *hostAuthPhysical
	if existingEntry != nil {
		existing, err = hostAuthGetPhysical(existingEntry.AuthIndex)
		if err != nil {
			return map[string]any{"success": false, "error": "host.auth.get: " + err.Error()}
		}
	}
	var fileJSON []byte
	if existing != nil {
		fileJSON, err = mergeAuthStorageJSON(existing.JSON, sa)
		if err == nil {
			var doc map[string]any
			if err = json.Unmarshal(fileJSON, &doc); err == nil {
				doc["type"] = providerName
				doc["provider"] = providerName
				doc["logo"] = pluginLogoURL
				fileJSON, err = json.Marshal(doc)
			}
		}
	} else {
		// New imports receive the provider defaults. Existing imports above keep
		// disabled/note/custom host fields from the physical file unchanged.
		fileJSON, err = buildAuthFileJSON(sa, false, displayNote(sa, nil, false), nil)
	}
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	saveResp, err := hostAuthSaveJSONResponse(auth.FileName, fileJSON)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	if saveResp.Name == "" {
		saveResp.Name = auth.FileName
	}
	if saveResp.Path == "" && existing != nil {
		saveResp.Path = existing.Path
	}
	// When a UID-bearing legacy file was matched, canonicalize it only after
	// the new save succeeds. Directory confinement keeps the cleanup bounded.
	if existing != nil && isLegacyWorkbuddyAuthName(existing.Name) &&
		!strings.EqualFold(existing.Name, auth.FileName) && existing.Path != "" {
		_ = deleteAuthFileInDir(existing.Path, filepath.Dir(existing.Path))
	}
	return map[string]any{
		"success":  true,
		"name":     saveResp.Name,
		"path":     saveResp.Path,
		"uid":      sa.Account.UID,
		"nickname": sa.Account.Nickname,
		"file":     auth.FileName,
	}
}

func handleCheckinConfig(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	_ = json.Unmarshal(req.Body, &body)
	checkinAutoMu.Lock()
	if body.Enabled != nil {
		// Runtime-only toggle: the CPA host exposes no plugin-config write
		// callback, so persisting would mean editing the host's config.yaml
		// from inside the plugin (fragile under docker volume mounts). The
		// value from config_yaml wins again on CPA restart.
		checkinAuto = *body.Enabled
	}
	cur := checkinAuto
	checkinAutoMu.Unlock()
	return map[string]any{"checkin_auto": cur, "persistent": false}
}

// handleClaimTrial claims the expert trial pack for one Global account.
// CN accounts are rejected — the trial endpoint is Global-only.
func handleClaimTrial(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, f := range files {
		if f.AuthIndex != authIndex {
			continue
		}
		// Trial is a one-shot POST. Hold the account mutation lock across the
		// read and claim so a concurrent manual/scheduled action cannot issue a
		// second claim. Reconcile after unlocking because it takes the same lock.
		unlock := lockAuthMutation(f.AuthIndex, f.ID)
		var (
			sa  *storedAuth
			out map[string]any
		)
		func() {
			defer unlock()
			var err error
			sa, err = hostAuthGet(f.AuthIndex)
			if err != nil {
				out = map[string]any{"auth_index": authIndex, "error": err.Error()}
				return
			}
			if !isGlobalDomain(sa.Auth.Domain) {
				out = map[string]any{"auth_index": authIndex, "error": "专家加油包仅适用于国际版账号"}
				return
			}
			res, callErr := performTrialCall(sa)
			out = map[string]any{"auth_index": authIndex, "nickname": sa.Account.Nickname}
			if callErr != nil {
				out["error"] = callErr.Error()
			} else {
				for k, v := range res {
					out[k] = v
				}
				// Upstream answered "has applied trial": remember it so the panel
				// stops offering a claim that cannot succeed. The pack vanishes
				// from the credits list after 14 days, so this memory is the only
				// way to tell "never got it" from "got it and it expired".
				if claimed, _ := out["already_claimed"].(bool); claimed {
					rememberTrialClaimed(authIndex)
				}
			}
			// Invalidate credits cache (copy entry, set credits=nil, keep plan/checkin).
			if v, ok := accountCache.Load(f.ID); ok {
				if e, ok2 := v.(*accountCacheEntry); ok2 {
					fresh := *e
					fresh.credits = nil
					fresh.fetched = time.Now()
					accountCache.Store(f.ID, &fresh)
				}
			}
		}()
		if sa != nil && lifecycleEnabled() {
			_, _ = reconcileOneAccount(authIndex, f.ID, true)
		}
		return out
	}
	return map[string]any{"error": "account not found"}
}

// handleCreditsQuery returns real-time credits for one or all accounts.
// Pass ?auth_index=<idx> to query a single account; omit for all.
// Single-account mode returns full account info (nickname, region, credits,
// exhausted, trial_claimed) so the panel can update one card without
// reloading the entire dashboard.
func handleCreditsQuery(req pluginapi.ManagementRequest) map[string]any {
	authIndex := ""
	if vals := req.Query["auth_index"]; len(vals) > 0 {
		authIndex = strings.TrimSpace(vals[0])
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Single-account: return one full account row (like dashboard entry).
	if authIndex != "" {
		for _, f := range files {
			if f.AuthIndex != authIndex {
				continue
			}
			sa, err := hostAuthGet(f.AuthIndex)
			if err != nil {
				return map[string]any{"accounts": []map[string]any{{
					"auth_index": authIndex, "error": "load auth: " + err.Error(),
				}}}
			}
			// One explicit refresh must update EVERYTHING the row shows, not
			// just credits: cachedAccountDetails fetches credits, plan,
			// check-in and the growth-day state in parallel (singleflight) and
			// stores them together. Fetching credits alone here left the Daily
			// column stale, and the hand-rolled cache write dropped the daily
			// state entirely.
			plan, ci, cr, errs := cachedAccountDetails(f.ID, sa, true)
			acct := map[string]any{
				"auth_index": authIndex,
				"nickname":   sa.Account.Nickname,
				"uid":        sa.Account.UID,
				"region":     accountRegion(sa),
				"name":       f.Name,
				"label":      f.Label,
				"disabled":   f.Disabled,
				"plan":       plan,
				"credits":    cr,
			}
			if ci != nil {
				acct["checkin"] = ci
			}
			if len(errs) > 0 {
				acct["error"] = strings.Join(errs, "; ")
			}
			if v, ok := accountCache.Load(f.ID); ok {
				if e, ok2 := v.(*accountCacheEntry); ok2 && e != nil && e.daily != nil {
					acct["daily"] = e.daily
				}
			}
			acct["exhausted"] = isCreditsExhausted(cr)
			if isGlobalDomain(sa.Auth.Domain) {
				acct["trial_claimed"] = hasTrialPack(cr) || trialClaimedRemembered(authIndex)
			}
			return map[string]any{"accounts": []map[string]any{acct}}
		}
		return map[string]any{"error": "account not found"}
	}
	// All accounts: return simplified list.
	type acctCredits struct {
		AuthIndex string          `json:"auth_index"`
		Nickname  string          `json:"nickname"`
		UID       string          `json:"uid"`
		Credits   *creditsSummary `json:"credits,omitempty"`
		Error     string          `json:"error,omitempty"`
	}
	var out []acctCredits
	for _, f := range files {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			out = append(out, acctCredits{AuthIndex: f.AuthIndex, Error: "load auth: " + err.Error()})
			continue
		}
		cr, err := fetchUserResource(sa)
		ac := acctCredits{AuthIndex: f.AuthIndex, Nickname: sa.Account.Nickname, UID: sa.Account.UID}
		if err != nil {
			ac.Error = err.Error()
		} else {
			ac.Credits = cr
		}
		out = append(out, ac)
	}
	return map[string]any{"accounts": out}
}
