// panel.go serves the management dashboard: the aggregated account list the
// web UI consumes (buildDashboardEx) and the embedded HTML page itself.
package main

import (
	_ "embed"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// wbAccount is one row of the dashboard.
type wbAccount struct {
	AuthIndex          string `json:"auth_index"`
	AuthID             string `json:"auth_id,omitempty"`
	Name               string `json:"name"`
	Label              string `json:"label"`
	Nickname           string `json:"nickname"`
	UID                string `json:"uid"`
	Region             string `json:"region"` // "cn" or "global"
	Plan               string `json:"plan"`
	Status             string `json:"status"`
	Disabled           bool   `json:"disabled"`
	Exhausted          bool   `json:"exhausted"`
	ConcurrencyCurrent int    `json:"concurrency_current"`
	ConcurrencyLimit   int    `json:"concurrency_limit"`
	ConcurrencyKnown   bool   `json:"concurrency_known"`
	// Daily carries the last growth-report result for this account (Global
	// only). Nil means "no run recorded yet", which the panel shows as unknown
	// rather than claiming the account has not reported.
	Daily *dailyStatus `json:"daily,omitempty"`
	// ConcurrencyKey is the gate key this account's slots are reported under by
	// GET /concurrency (realm:UID). The badge polls that endpoint, so it must
	// look the account up by this key — auth_index is a different namespace and
	// silently matched nothing, pinning every badge at 0.
	ConcurrencyKey string          `json:"concurrency_key,omitempty"`
	Credits        *creditsSummary `json:"credits,omitempty"`
	Checkin        *checkinSummary `json:"checkin,omitempty"`
	TrialClaimed   bool            `json:"trial_claimed,omitempty"` // Global: expert trial already claimed
	Error          string          `json:"error,omitempty"`
}

// credits/checkin/plan fields are left empty — the panel renders skeletons
// and fetches them lazily via /credits?auth_index=<idx>. This avoids hitting
// upstream billing APIs for all accounts simultaneously on page load (which
// causes 500 from rate-limited /v2/billing/meter/get-user-resource).
// dailyStatus is the per-account daily-report view.
//
// Lit/Score come from the growth heatmap (authoritative: it says whether today
// already counts, whoever sent the beacon). Status/Detail come from our own
// report run when we have one, so a failed send is visible too. Accounts with
// neither show no daily block at all.
type dailyStatus struct {
	Day     string   `json:"day,omitempty"`
	Score   int      `json:"score,omitempty"`
	Lit     bool     `json:"lit"`
	LastLit string   `json:"last_lit,omitempty"`
	LitDays []string `json:"lit_days,omitempty"`
	Status  string   `json:"status,omitempty"`
	Detail  string   `json:"detail,omitempty"`
	When    string   `json:"when,omitempty"`
	Source  string   `json:"source,omitempty"` // "heatmap" | "run"
}

// dailyStatusFor maps a heatmap read to a short status word.
func dailyStatusFor(d *growthDaily) string {
	if d == nil {
		return ""
	}
	if d.Lit {
		return "already-lit"
	}
	return "not-lit"
}

// dailyStatusByAuth merges the live heatmap read (from the account cache) with
// our own last report run. The heatmap wins on lit/score because the host's own
// systemd timer may be the one that lit the day; the run record only adds the
// status of OUR attempt (reported / already-lit / failed).
func dailyStatusByAuth() map[string]dailyStatus {
	out := map[string]dailyStatus{}

	if sum := getLastDailyBonus(); sum != nil {
		when := sum.When.Format(time.RFC3339)
		for _, r := range sum.Results {
			if r.AuthIndex == "" {
				continue
			}
			out[r.AuthIndex] = dailyStatus{
				Day: r.Day, Score: r.Score, Lit: r.Lit,
				Status: r.Status, Detail: r.Detail, When: when, Source: "run",
			}
		}
	}

	// Live heatmap state, keyed by auth_index like the panel rows.
	files, err := loadAuthList()
	if err == nil {
		for _, f := range files {
			v, ok := accountCache.Load(f.ID)
			if !ok {
				continue
			}
			e, ok := v.(*accountCacheEntry)
			if !ok || e == nil || e.daily == nil || e.daily.Err != "" {
				continue
			}
			ds := out[f.AuthIndex]
			ds.Lit = e.daily.Lit
			ds.Score = e.daily.Score
			if e.daily.Today != "" {
				ds.Day = e.daily.Today
			}
			ds.LastLit = e.daily.LastLit
			ds.LitDays = e.daily.LitDays
			ds.Source = "heatmap"
			// Keep our own attempt status (e.g. "failed"); the heatmap only
			// knows the outcome.
			if ds.Status == "" {
				ds.Status = dailyStatusFor(e.daily)
			}
			out[f.AuthIndex] = ds
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func buildDashboardEx(force, fetchCredits bool) map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Prune cache entries for accounts that no longer exist (auth deleted via
	// CPA UI) or whose TTL expired long ago. Without this, accountCache grows
	// monotonically for the lifetime of the process.
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
	}
	accountCache.Range(func(key, value any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			accountCache.Delete(key)
			lifecycleState.Delete(key)
			return true
		}
		if e, ok := value.(*accountCacheEntry); ok && time.Since(e.fetched) > 4*accountCacheTTL {
			accountCache.Delete(key)
		}
		return true
	})
	// Also prune stale lifecycle state and checkin locks for gone accounts.
	pruneLifecycleState()
	pruneCheckinLocks()
	out := make([]wbAccount, len(files))
	inFlight := accountInFlightSnapshot()
	dailyByAuth := dailyStatusByAuth()
	// Accounts are independent — fetch their dashboards concurrently. With 4
	// accounts this cuts cold-load latency from ~4×(3 serial upstream calls)
	// to roughly one slowest account.
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			acct := wbAccount{
				AuthIndex:        f.AuthIndex,
				AuthID:           f.ID,
				Name:             f.Name,
				Label:            f.Label,
				Status:           f.Status,
				Disabled:         f.Disabled,
				ConcurrencyLimit: maxAccountChatInFlight,
			}
			sa, phys, err := hostAuthGetBundle(f.AuthIndex)
			if err != nil {
				acct.Error = "load auth: " + err.Error()
				out[i] = acct
				return
			}
			// Physical file is source of truth for disabled (host list may lag).
			if phys != nil {
				acct.Disabled = phys.Disabled
				if phys.Name != "" {
					acct.Name = phys.Name
				}
			}
			acct.Nickname = sa.Account.Nickname
			acct.UID = sa.Account.UID
			acct.Region = accountRegion(sa)
			gateKey := accountGateKey(&failoverCandidate{AuthID: f.AuthIndex, Auth: sa})
			acct.ConcurrencyCurrent = inFlight[gateKey]
			acct.ConcurrencyKey = gateKey
			acct.ConcurrencyKnown = true
			if isGlobalDomain(sa.Auth.Domain) && trialClaimedRemembered(f.AuthIndex) {
				// No cached credits at all, but we know the pack was used.
				acct.TrialClaimed = true
			}
			if ds, ok := dailyByAuth[f.AuthIndex]; ok {
				copy := ds
				acct.Daily = &copy
			}
			if fetchCredits {
				plan, ci, cr, errs := cachedAccountDetails(f.ID, sa, force)
				acct.Plan = plan
				acct.Checkin = ci
				acct.Credits = cr
				acct.Exhausted = isCreditsExhausted(cr)
				// cachedAccountDetails just refreshed the entry, including the
				// growth-heatmap read. Read it back so this response carries the
				// daily state instead of only the next (cache-warm) one.
				if v, ok := accountCache.Load(f.ID); ok {
					if e, ok2 := v.(*accountCacheEntry); ok2 && e != nil && e.daily != nil && e.daily.Err == "" {
						acct.Daily = &dailyStatus{
							Day: e.daily.Today, Score: e.daily.Score, Lit: e.daily.Lit,
							LastLit: e.daily.LastLit, LitDays: e.daily.LitDays,
							Status: dailyStatusFor(e.daily), Source: "heatmap",
						}
					}
				}
				if isGlobalDomain(sa.Auth.Domain) {
					acct.TrialClaimed = hasTrialPack(cr) || trialClaimedRemembered(f.AuthIndex)
				}
				// Keep note in sync (throttled); do not block dashboard on save errors.
				_ = syncAuthNote(f.AuthIndex, f.ID, sa, cr, acct.Disabled)
				acct.Error = strings.Join(errs, "; ")
			} else {
				// Light load: use cached values if available, but don't fetch upstream.
				if v, ok := accountCache.Load(f.ID); ok {
					if e, ok2 := v.(*accountCacheEntry); ok2 {
						acct.Plan = e.plan
						acct.Checkin = e.checkin
						acct.Credits = e.credits
						acct.Exhausted = isCreditsExhausted(e.credits)
						if isGlobalDomain(sa.Auth.Domain) {
							// The remembered marker matters most here: a light load
							// often has no credits yet, and an expired pack leaves
							// no trace in the list.
							acct.TrialClaimed = hasTrialPack(e.credits) || trialClaimedRemembered(f.AuthIndex)
						}
					}
				}
			}
			out[i] = acct
		}(i, f)
	}
	wg.Wait()
	// After refresh (force), run lifecycle so exhaust→disable/delete is immediate.
	var life []map[string]any
	if force && lifecycleEnabled() {
		life = reconcileAllAccounts(true)
		// Drop accounts deleted during reconcile (Global exhaust) and refresh
		// disabled/exhausted from disk/cache (host list may lag after save).
		if files2, err2 := hostAuthList(); err2 == nil {
			live := make(map[string]struct{}, len(files2))
			disabledBy := make(map[string]bool, len(files2))
			for _, f := range files2 {
				live[f.AuthIndex] = struct{}{}
				// Prefer host list Disabled after reconcile; avoids N extra host.auth.get.
				// Dashboard row load already used hostAuthGetBundle for physical truth.
				disabledBy[f.AuthIndex] = f.Disabled
			}
			filtered := out[:0]
			for _, a := range out {
				if _, ok := live[a.AuthIndex]; !ok {
					continue
				}
				if d, ok := disabledBy[a.AuthIndex]; ok {
					a.Disabled = d
				}
				// Credits may have been refreshed during reconcile — re-read cache.
				if v, ok := accountCache.Load(a.AuthID); ok {
					if e, ok2 := v.(*accountCacheEntry); ok2 {
						if e.credits != nil {
							a.Credits = e.credits
							a.Exhausted = isCreditsExhausted(e.credits)
						}
						if e.plan != "" {
							a.Plan = e.plan
						}
						if e.checkin != nil {
							a.Checkin = e.checkin
						}
					}
				}
				filtered = append(filtered, a)
			}
			out = filtered
		}
	}
	checkinAutoMu.RLock()
	auto := checkinAuto
	checkinAutoMu.RUnlock()
	// Aggregate credits for panel/API consumers (all accounts currently in out).
	sum := summarizeCredits(out)
	resp := map[string]any{
		"accounts":       out,
		"checkin_auto":   auto,
		"daily_bonus":    dailyBonusEnabled(),
		"oauth_region":   oauthRegionValue(),
		"lifecycle_auto": lifecycleEnabled(),
		"schedule":       []string{"09:00", "21:00"},
		"server_time":    time.Now().Format("2006-01-02 15:04:05"),
		"summary":        sum,
	}
	if len(life) > 0 {
		resp["lifecycle"] = life
	}
	return resp
}

// summarizeCredits aggregates remain/used across dashboard accounts.
func summarizeCredits(accounts []wbAccount) map[string]any {
	var remain, used, size, cnRemain, cnUsed, cnSize, glRemain, glUsed, glSize int64
	var known, disabledN, exhaustedN, packs int
	for _, a := range accounts {
		if a.Disabled {
			disabledN++
		}
		if a.Exhausted {
			exhaustedN++
		}
		if a.Credits == nil {
			continue
		}
		cr := a.Credits
		if cr.TotalRemain == 0 && cr.TotalUsed == 0 && cr.TotalSize == 0 && len(cr.Packages) == 0 {
			continue
		}
		known++
		remain += cr.TotalRemain
		used += cr.TotalUsed
		size += cr.TotalSize
		packs += cr.PackCount
		if a.Region == "global" {
			glRemain += cr.TotalRemain
			glUsed += cr.TotalUsed
			glSize += cr.TotalSize
		} else {
			cnRemain += cr.TotalRemain
			cnUsed += cr.TotalUsed
			cnSize += cr.TotalSize
		}
	}
	total := remain + used
	if size > total {
		total = size
	}
	return map[string]any{
		"account_count":   len(accounts),
		"known_count":     known,
		"disabled_count":  disabledN,
		"exhausted_count": exhaustedN,
		"pack_count":      packs,
		"total_remain":    remain,
		"total_used":      used,
		"total_size":      size,
		"total":           total,
		"cn_remain":       cnRemain,
		"cn_used":         cnUsed,
		"cn_size":         cnSize,
		"global_remain":   glRemain,
		"global_used":     glUsed,
		"global_size":     glSize,
	}
}

// Web panel (self-contained HTML, no external assets)
// -----------------------------------------------------------------------------

func servePanel(sub string) []byte {
	if sub != "" && sub != "/" && sub != "/panel" && sub != "/panel.html" {
		return []byte("<h1>404</h1>")
	}
	if pluginPanelBrand == "CodeBuddy" {
		return panelHTML
	}
	// Keep the production panel asset shared while giving isolated test builds
	// an unmistakable label in the browser and sidebar.
	body := string(panelHTML)
	body = strings.ReplaceAll(body, "CodeBuddy", pluginPanelBrand)
	body = strings.ReplaceAll(body, "/v0/management/plugins/workbuddy", "/v0/management/plugins/"+pluginRouteID)
	return []byte(body)
}

//go:embed panel.html
var panelHTML []byte
