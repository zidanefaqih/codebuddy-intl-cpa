// usage_config.go decodes plugin config from config_yaml on every
// register/reconfigure call and resolves the CPAMP usage report URL/key.
// All plugin-level config lives here so the rest of the plugin reads
// consistent, lock-protected snapshots.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// check-in schedule: 09:00 and 21:00 local time.
var checkinHours = []int{9, 21}

// shouldRunCheckinNow reports whether now falls in a scheduled check-in
// window. schedulerLoop wakes for other task families too, so it must not call
// the CN check-in path on those unrelated ticks.
func shouldRunCheckinNow(now time.Time) bool {
	for _, h := range checkinHours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !now.Before(t) && now.Before(t.Add(time.Hour)) {
			return true
		}
	}
	return false
}

// plugin-level config decoded from plugin.register/reconfigure config_yaml.
var (
	checkinAuto   = true // enabled by default
	checkinAutoMu sync.RWMutex

	// usageReportURL / usageReportKey: POST NDJSON to CPA-Manager-Plus
	// /v0/management/usage/import (only path that reaches request monitoring;
	// c-shared plugins cannot use host usage.DefaultManager/redisqueue).
	//
	// Resolution order (community-style, like codex-auth-importer env injection):
	//  1) plugins.configs.workbuddy.usage_report_* in config.yaml
	//  2) env USAGE_REPORT_URL / USAGE_REPORT_KEY / CPAMP_ADMIN_KEY
	//  3) secret files (docker secrets / bind-mount), e.g. /run/secrets/cpamp_admin_key
	// Default URL targets the compose service name of CPA-Manager-Plus.
	usageReportURL = defaultUsageReportURL
	usageReportKey = ""
	usageReportMu  sync.RWMutex

	// managementAPIKey: plugin-layer auth for /v0/management/plugins/workbuddy/*
	// write endpoints. When empty, plugin relies on host-side auth (CPA's
	// management middleware) — that's the historical default and stays
	// backward-compatible. When set via config_yaml management_key: or env
	// WB_MANAGEMENT_KEY, handleManagement enforces constant-time Bearer match
	// plus per-IP token-bucket rate limiting on mutating endpoints.
	managementAPIKey   = ""
	managementAPIKeyMu sync.RWMutex

	// selfURL is the base URL the plugin uses to reach the host's own
	// management API (live model-registry reload, PLAN-0907b T1). The default
	// matches the production CPA port; dev/alternate ports override via plugin
	// config self_url or env WB_SELF_URL.
	selfURL   = defaultSelfURL
	selfURLMu sync.RWMutex

	// liveReload gates the post-save model-registry reload trigger. Default on;
	// set mm_live_reload: false in plugin config to disable.
	liveReload   = true
	liveReloadMu sync.RWMutex

	// oauthRegion selects which realm issues the OAuth login URL.
	// Config key oauth_region (intl | cn), env WB_OAUTH_REGION. Default intl:
	// the plugin targets Intl accounts, and a state issued on the CN host hands
	// the browser a copilot.tencent.com login page that redirects to
	// codebuddy.cn — the flow this plugin could never finish (issue #5).
	oauthRegion   = oauthRegionIntl
	oauthRegionMu sync.RWMutex
)

// OAuth login regions. The login state must be issued by the realm that owns
// the account being added, so the region is carried through start → poll; see
// loginEndpointsForRegion.
const (
	oauthRegionIntl = "intl"
	oauthRegionCN   = "cn"
)

// normalizeOAuthRegion maps user input onto a known region. Anything
// unrecognised resolves to Intl, never to CN: the CN issuer is the path that
// strands the plugin's target (Intl) audience, so a typo must not fall back
// onto it.
func normalizeOAuthRegion(v string) string {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`)) {
	case oauthRegionCN, "cn-domain", "china", "tencent", "copilot":
		return oauthRegionCN
	default:
		return oauthRegionIntl
	}
}

// oauthRegionValue returns the configured login region.
func oauthRegionValue() string {
	oauthRegionMu.RLock()
	defer oauthRegionMu.RUnlock()
	return oauthRegion
}

// setOAuthRegionRuntime applies a region chosen in the panel. Runtime-only,
// with the same contract as checkin_auto / daily_bonus: the CPA host exposes no
// plugin-config write callback, so the value from config_yaml wins again on
// restart. Reports ok=false for a value it does not recognise so the panel can
// surface an error instead of silently keeping the old region.
func setOAuthRegionRuntime(v string) (region string, ok bool) {
	raw := strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`))
	switch raw {
	case oauthRegionIntl, oauthRegionCN, "global", "international", "china", "tencent", "copilot":
	default:
		return oauthRegionValue(), false
	}
	next := normalizeOAuthRegion(raw)
	oauthRegionMu.Lock()
	oauthRegion = next
	oauthRegionMu.Unlock()
	return next, true
}

// defaultSelfURL targets the standard production CPA management port.
const defaultSelfURL = "http://127.0.0.1:8317"

// selfURLValue returns the configured host management base URL.
func selfURLValue() string {
	selfURLMu.RLock()
	defer selfURLMu.RUnlock()
	if strings.TrimSpace(selfURL) == "" {
		return defaultSelfURL
	}
	return selfURL
}

// liveReloadEnabled reports whether the plugin should trigger a host reload
// after a successful settings save.
func liveReloadEnabled() bool {
	liveReloadMu.RLock()
	defer liveReloadMu.RUnlock()
	return liveReload
}

// Default URL tries localhost first (works for both bare-metal and Docker
// host-network), falls back to Docker compose service name. The probe runs
// once at configure() time; a reachable endpoint wins.
//
// For users who run CPA Manager Plus on a different host/port, set
// usage_report_url in plugin config or env USAGE_REPORT_URL.
const defaultUsageReportURL = "http://127.0.0.1:18317/v0/management/usage/import"

const fallbackUsageReportURL = "http://cpa-manager-plus:18317/v0/management/usage/import"

// configure decodes plugin config from the lifecycle request.
func configure(raw []byte) {
	// Parse config without holding any lock (fixes nested-lock hazard).
	nextCheckinAuto := true
	nextLifecycleAuto := true
	nextKeepaliveAuto := true
	nextDailyBonusAuto := true
	nextMgmtKey := ""
	nextSelfURL := ""
	nextLiveReload := true
	nextOAuthRegion := ""

	cfgURL, cfgKey := "", ""
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err == nil {
			for _, line := range strings.Split(string(req.ConfigYAML), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "checkin_auto:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "checkin_auto:"))
					nextCheckinAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "lifecycle_auto:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "lifecycle_auto:"))
					v = strings.Trim(v, "\"'")
					nextLifecycleAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "usage_report_url:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "usage_report_url:"))
					cfgURL = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "usage_report_key:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "usage_report_key:"))
					cfgKey = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "management_key:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "management_key:"))
					nextMgmtKey = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "self_url:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "self_url:"))
					nextSelfURL = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "mm_live_reload:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "mm_live_reload:"))
					v = strings.Trim(v, "\"'")
					nextLiveReload = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "token_keepalive:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "token_keepalive:"))
					v = strings.Trim(v, "\"'")
					nextKeepaliveAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "daily_bonus:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "daily_bonus:"))
					v = strings.Trim(v, "\"'")
					nextDailyBonusAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "oauth_region:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "oauth_region:"))
					nextOAuthRegion = strings.Trim(v, "\"'")
				}
			}
		}
	}

	// Apply each setting under its own lock — no nesting.
	checkinAutoMu.Lock()
	checkinAuto = nextCheckinAuto
	checkinAutoMu.Unlock()

	lifecycleAutoMu.Lock()
	lifecycleAuto = nextLifecycleAuto
	lifecycleAutoMu.Unlock()

	keepaliveAutoMu.Lock()
	keepaliveAuto = nextKeepaliveAuto
	keepaliveAutoMu.Unlock()

	dailyBonusAutoMu.Lock()
	dailyBonusAuto = nextDailyBonusAuto
	dailyBonusAutoMu.Unlock()

	// management key: config_yaml > env > keep existing. Empty stays empty
	// (plugin-layer auth disabled, host middleware still guards).
	if nextMgmtKey == "" {
		nextMgmtKey = strings.TrimSpace(os.Getenv("WB_MANAGEMENT_KEY"))
	}
	managementAPIKeyMu.Lock()
	managementAPIKey = nextMgmtKey
	managementAPIKeyMu.Unlock()

	// self_url: config_yaml > env WB_SELF_URL > default (production port).
	if nextSelfURL == "" {
		nextSelfURL = strings.TrimSpace(os.Getenv("WB_SELF_URL"))
	}
	if nextSelfURL == "" {
		nextSelfURL = defaultSelfURL
	}
	selfURLMu.Lock()
	selfURL = nextSelfURL
	selfURLMu.Unlock()

	liveReloadMu.Lock()
	liveReload = nextLiveReload
	liveReloadMu.Unlock()

	// oauth_region: config_yaml > env WB_OAUTH_REGION > default (intl). The
	// region decides which realm issues the login URL; see
	// loginEndpointsForRegion. An unrecognised value resolves to intl, never to
	// cn: the CN issuer is what strands an Intl account (issue #5).
	if nextOAuthRegion == "" {
		nextOAuthRegion = strings.TrimSpace(os.Getenv("WB_OAUTH_REGION"))
	}
	oauthRegionMu.Lock()
	oauthRegion = normalizeOAuthRegion(nextOAuthRegion)
	oauthRegionMu.Unlock()

	resolveUsageReport(cfgURL, cfgKey)
	ensureScheduler()
	if n := mmDropLegacyOverrides(); n > 0 {
		guardLogf("model-manager: removed %d legacy per-account override(s) from the state file (feature retired)", n)
	}
}

// resolveUsageReport fills usageReportURL/key from config → env → secret files.
// Mirrors community plugins that inject management keys via env/build (e.g.
// codex-auth-importer CODEX_AUTH_IMPORTER_MANAGEMENT_KEY), not plaintext CPA
// remote-management.secret-key (that field is bcrypt-hashed).
func resolveUsageReport(cfgURL, cfgKey string) {
	url := firstNonEmpty(
		strings.TrimSpace(cfgURL),
		strings.TrimSpace(os.Getenv("USAGE_REPORT_URL")),
		strings.TrimSpace(os.Getenv("CPAMP_USAGE_IMPORT_URL")),
	)
	if url == "" {
		url = probeUsageReportURL()
	}
	key := firstNonEmpty(
		strings.TrimSpace(cfgKey),
		strings.TrimSpace(os.Getenv("USAGE_REPORT_KEY")),
		strings.TrimSpace(os.Getenv("CPAMP_ADMIN_KEY")),
		strings.TrimSpace(os.Getenv("CPA_MANAGER_ADMIN_KEY")),
		readSecretFile(os.Getenv("USAGE_REPORT_KEY_FILE")),
		readSecretFile(os.Getenv("CPAMP_ADMIN_KEY_FILE")),
		readSecretFile(os.Getenv("CPA_MANAGER_ADMIN_KEY_FILE")),
		// docker compose secrets default path
		readSecretFile("/run/secrets/cpamp_admin_key"),
		readSecretFile("/run/secrets/cpamp-admin-key"),
		// optional bind-mounts used on this host
		readSecretFile("/CLIProxyAPI/secrets/cpamp-admin-key"),
		readSecretFile("/CLIProxyAPI/secrets/cpamp_admin_key"),
	)
	usageReportMu.Lock()
	usageReportURL = url
	usageReportKey = key
	usageReportMu.Unlock()
}

// probeUsageReportURL tries localhost first (bare-metal + Docker host-network),
// then Docker compose service name. Returns whichever responds; defaults to
// localhost if both fail (better to try localhost than an unreachable hostname).
func probeUsageReportURL() string {
	for _, candidate := range []string{defaultUsageReportURL, fallbackUsageReportURL} {
		if probeURL(candidate, 2*time.Second) {
			return candidate
		}
	}
	return defaultUsageReportURL
}

// probeURL does a quick HEAD/GET to check if the endpoint is reachable.
func probeURL(target string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(target)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Any HTTP response (even 401) means the endpoint is reachable;
	// connection refused / DNS failure means not reachable.
	return resp.StatusCode > 0
}

func readSecretFile(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
