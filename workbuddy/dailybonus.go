// dailybonus.go implements the Global-realm daily credit reward.
//
// WHY THIS EXISTS
// ---------------
// A Global (Intl) CodeBuddy account has no daily check-in: the CN "Buddy
// gas-station" action is a desktop-app deep link (workbuddy://expert), so
// checkin-activity-status answers active:false forever. The plugin ecosystem
// therefore treats Global as having no daily income at all — mmqz/cpa-multi-plugins
// gates it out with "the growth center only exists for CN accounts", and
// Lxapk/workbuddy-cpa-plugin does the same with "the growth centre only exists
// for the domestic realm".
//
// That is wrong. Global realms serve the whole growth-centre endpoint family
// (heatmap, streak, tasks, energy, lottery, buddy) at the same billing host.
// The income path is the activity beacon the official client sends on every
// chat: one `chat_request_send` event to POST /v2/report. A day with a non-zero
// `score` in GET /activity/growth/heatmap earns a "Bonus Pack" of 30 credits
// (package TCACA_code_007_nzdH5h4Nl0 = 平台奖励积分, valid 30 days) which is
// granted automatically on the NEXT China-time day.
//
// Verified 2026-09-28 → 2026-09-29 across 7 accounts: six Global accounts lit by
// this report alone each received their 30-credit pack the following morning
// (02:14–02:21 local), plus one account that was lit by a plain client login.
// Consecutive days also unlock streak tiers (14d → +50, 28d → +150 credits).
//
// DESIGN
//   - Idempotent per account per day. "Today" is read from the API's own
//     heatmap (the last cell), never from local time, so there is no timezone
//     arithmetic and China-time day boundaries need no special handling.
//   - The report POST is deliberately NOT retried. The event is day-idempotent
//     upstream, but a blind resend would double-count the day (score 2 → 4).
//     A failed send is picked up by the next scheduled tick instead.
//   - Several ticks per day (dailyBonusHours). Because the runner skips days
//     that are already lit, extra ticks are free and act as a safety net.
//   - Global accounts only. CN accounts keep using the check-in path, and their
//     growth domain differs (copilot.tencent.com), which this file does not
//     implement.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// growthHeatmapPath is the read-only "is this day lit" probe. It carries no
	// /v2 prefix — the growth family is served straight off the billing host.
	growthHeatmapPath = "/activity/growth/heatmap"
	// growthReportPath is the activity beacon. Unlike the other growth paths
	// this one lives under /v2.
	growthReportPath = "/v2/report"
)

// dailyBonusHours is the daily schedule (local time). Four ticks: the runner is
// idempotent, so running often only costs a heatmap GET per account once the day
// is lit, and a tick that fails (e.g. network not up yet on boot) is covered by
// the next one.
var dailyBonusHours = []int{8, 12, 16, 20}

// The beacon payload mirrors what the official client sends. The model fields
// are part of upstream's "experience this model" task judgement, so a plain
// default is used rather than whatever the user happens to be chatting with.
const (
	growthReportModelID   = "deepseek-v4-flash"
	growthReportModelName = "DeepSeek V4 Flash"
	growthReportInputLen  = 12
)

var (
	dailyBonusAuto   = true
	dailyBonusAutoMu sync.RWMutex
)

func dailyBonusEnabled() bool {
	dailyBonusAutoMu.RLock()
	defer dailyBonusAutoMu.RUnlock()
	return dailyBonusAuto
}

// growthClientToken mints a UUID-v4-shaped idempotency token. The upstream event
// carries a caller-generated conversation/request id; the reference
// implementations generate one per report, so each day looks like a fresh
// conversation rather than a replay.
func growthClientToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not recoverable here; fall back to a
		// time-derived token rather than reporting a zero UUID.
		return fmt.Sprintf("wb-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// growthChatEvent is one entry of the POST /v2/report array. Field names match
// the upstream JSON exactly (camelCase); the zero-value slices must still be
// present in the payload, so they are typed []any rather than omitted.
type growthChatEvent struct {
	EventCode            string `json:"eventCode"`
	Timestamp            int64  `json:"timestamp"`
	Mode                 string `json:"mode"`
	ConversationID       string `json:"conversationId"`
	RequestID            string `json:"requestId"`
	InputLength          int    `json:"inputLength"`
	RequestModelID       string `json:"requestModelId"`
	RequestModelName     string `json:"requestModelName"`
	MentionContexts      []any  `json:"mentionContexts"`
	KnowledgeID          []any  `json:"knowledgeId"`
	KnowledgeName        []any  `json:"knowledgeName"`
	PresentAt            int64  `json:"presentAt"`
	RootRequestID        string `json:"rootRequestId"`
	ParentConversationID string `json:"parentConversationId"`
	AgentName            string `json:"agentName"`
	AgentType            string `json:"agentType"`
	UserID               string `json:"userId"`
}

type growthHeatmapCell struct {
	Date        string `json:"date"`
	Score       int    `json:"score"`
	HasNewBuddy bool   `json:"has_new_buddy"`
}

type growthHeatmap struct {
	Cells []growthHeatmapCell `json:"cells"`
}

// growthGet performs a GET against the growth family on the account's billing
// host. billingCallOnce() cannot be reused here because it always issues POST,
// and the heatmap only answers GET.
func growthGet(sa *storedAuth, path string) (json.RawMessage, error) {
	base := billingBaseFor(sa)
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	raw := resp.Body
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, path, truncateRedacted(redactSecrets(string(raw)), 120))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Non-JSON here usually means the gateway bounced us to an HTML error
		// page (session dead / wrong realm host) — surface a snippet so the log
		// says which, instead of a bare "parse failed".
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncateRedacted(redactSecrets(string(raw)), 120))
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, nil
}

// fetchGrowthToday returns the current day's heatmap cell for the account plus
// the full list of lit dates (newest last). The last cell is the API's own
// notion of "today" in China time.
func fetchGrowthToday(sa *storedAuth) (growthHeatmapCell, []string, error) {
	data, err := growthGet(sa, growthHeatmapPath)
	if err != nil {
		return growthHeatmapCell{}, nil, err
	}
	var hm growthHeatmap
	if err := json.Unmarshal(data, &hm); err != nil {
		return growthHeatmapCell{}, nil, fmt.Errorf("heatmap decode: %w", err)
	}
	if len(hm.Cells) == 0 {
		return growthHeatmapCell{}, nil, fmt.Errorf("heatmap empty")
	}
	var lit []string
	for _, c := range hm.Cells {
		if c.Score > 0 {
			lit = append(lit, c.Date)
		}
	}
	return hm.Cells[len(hm.Cells)-1], lit, nil
}

// sendGrowthActivity posts one chat_request_send event.
//
// billingCallOnce is used instead of billingCall on purpose: the retry wrapper
// would resend on a transient error, and a duplicate event skews the day's
// counter (score 2 → 4). The day stays unlit until the next tick, which is
// harmless because several ticks are scheduled.
func sendGrowthActivity(sa *storedAuth) error {
	conv := "wb-" + growthClientToken()
	now := time.Now().UnixMilli()
	ev := growthChatEvent{
		EventCode:            "chat_request_send",
		Timestamp:            now,
		Mode:                 "craft",
		ConversationID:       conv,
		RequestID:            conv,
		InputLength:          growthReportInputLen,
		RequestModelID:       growthReportModelID,
		RequestModelName:     growthReportModelName,
		MentionContexts:      []any{},
		KnowledgeID:          []any{},
		KnowledgeName:        []any{},
		PresentAt:            now,
		RootRequestID:        conv,
		ParentConversationID: conv,
		AgentName:            "default",
		AgentType:            "conversation",
		UserID:               sa.Account.UID,
	}
	raw, err := json.Marshal([]growthChatEvent{ev})
	if err != nil {
		return err
	}
	_, err = billingCallOnce(sa, growthReportPath, json.RawMessage(raw))
	return err
}

// -----------------------------------------------------------------------------
// Run summary (observability, same shape as keepaliveSummary)
// -----------------------------------------------------------------------------

type dailyBonusSummary struct {
	When    time.Time       `json:"when"`
	Results []dailyBonusRow `json:"results"`
}

type dailyBonusRow struct {
	AuthIndex string   `json:"auth_index"`
	Nickname  string   `json:"nickname,omitempty"`
	Region    string   `json:"region"`
	Day       string   `json:"day,omitempty"`
	Score     int      `json:"score,omitempty"`
	Lit       bool     `json:"lit"`
	LitDates  []string `json:"lit_dates,omitempty"`
	Status    string   `json:"status"` // reported | already-lit | skipped | failed
	Detail    string   `json:"detail,omitempty"`
}

var (
	lastDailyBonusMu sync.RWMutex
	lastDailyBonus   *dailyBonusSummary
)

func recordDailyBonus(s *dailyBonusSummary) {
	lastDailyBonusMu.Lock()
	lastDailyBonus = s
	lastDailyBonusMu.Unlock()
}

func getLastDailyBonus() *dailyBonusSummary {
	lastDailyBonusMu.RLock()
	defer lastDailyBonusMu.RUnlock()
	return lastDailyBonus
}

// runDailyBonusOne lights one account's day if it is not lit yet.
func runDailyBonusOne(authIndex string, row dailyBonusRow) dailyBonusRow {
	sa, err := hostAuthGet(authIndex)
	if err != nil {
		row.Status, row.Detail = "failed", "get auth: "+truncateRedacted(err.Error(), 160)
		return row
	}
	row.Nickname = sa.Account.Nickname
	row.Region = accountRegion(sa)

	// Global only: the CN reward path is the check-in flow, and CN growth lives
	// on a different host (copilot.tencent.com) that this file does not target.
	if !isGlobalDomain(sa.Auth.Domain) {
		row.Status = "skipped"
		row.Detail = "not a Global account"
		return row
	}

	today, lit, err := fetchGrowthToday(sa)
	if err != nil {
		row.Status, row.Detail = "failed", "heatmap: "+truncateRedacted(err.Error(), 160)
		return row
	}
	row.Day, row.Score, row.LitDates = today.Date, today.Score, lit

	if today.Score > 0 {
		row.Lit = true
		row.Status = "already-lit"
		return row
	}
	if err := sendGrowthActivity(sa); err != nil {
		row.Status, row.Detail = "failed", "report: "+truncateRedacted(err.Error(), 160)
		return row
	}
	// The heatmap is aggregated and lags a successful report by a few seconds,
	// so a single immediate re-read can still show 0. Best-effort re-read for
	// the log only — the day is considered done either way, and the next tick
	// will see it lit.
	if cell, _, err := fetchGrowthToday(sa); err == nil {
		row.Score = cell.Score
		row.Lit = cell.Score > 0
	}
	row.Status = "reported"
	return row
}

// runDailyBonus processes every workbuddy auth in parallel (bounded), same
// concurrency shape as runTokenKeepalive.
func runDailyBonus() *dailyBonusSummary {
	sum := &dailyBonusSummary{When: time.Now()}
	files, err := hostAuthList()
	if err != nil {
		sum.Results = append(sum.Results, dailyBonusRow{Status: "failed", Detail: err.Error()})
		recordDailyBonus(sum)
		return sum
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var mu sync.Mutex
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := runDailyBonusOne(f.AuthIndex, dailyBonusRow{AuthIndex: f.AuthIndex})
			mu.Lock()
			sum.Results = append(sum.Results, row)
			mu.Unlock()
		}()
	}
	wg.Wait()
	recordDailyBonus(sum)
	return sum
}

// -----------------------------------------------------------------------------
// Schedule + management surface
// -----------------------------------------------------------------------------

// nextDailyBonusTime mirrors nextKeepaliveTime for dailyBonusHours.
func nextDailyBonusTime(now time.Time) time.Time {
	var earliest time.Time
	for _, h := range dailyBonusHours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// shouldRunDailyBonusNow reports whether now falls in the hour after any
// scheduled slot. Used by schedulerLoop so the bonus fires on the same tick as
// checkin/keepalive without needing its own timer.
func shouldRunDailyBonusNow(now time.Time) bool {
	for _, h := range dailyBonusHours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !now.Before(t) && now.Before(t.Add(time.Hour)) {
			return true
		}
	}
	return false
}

// handleDailyBonusNow triggers a manual run (all accounts, or one when the body
// carries auth_index). Manual runs ignore the daily_bonus toggle — the toggle
// gates only the scheduled runs.
func handleDailyBonusNow(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		sum := runDailyBonus()
		return map[string]any{"when": sum.When, "results": sum.Results}
	}
	row := runDailyBonusOne(authIndex, dailyBonusRow{AuthIndex: authIndex})
	return map[string]any{"when": time.Now(), "results": []dailyBonusRow{row}}
}

// handleDailyBonusStatus returns the last run plus the current schedule/toggle.
func handleDailyBonusStatus() map[string]any {
	return map[string]any{
		"enabled":  dailyBonusEnabled(),
		"schedule": dailyBonusHours,
		"last_run": getLastDailyBonus(),
	}
}

// handleDailyBonusConfig toggles the scheduled run. Like the check-in toggle it
// is runtime-only: the host exposes no plugin-config write callback, so the
// value from config_yaml wins again on restart.
func handleDailyBonusConfig(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	_ = json.Unmarshal(req.Body, &body)
	dailyBonusAutoMu.Lock()
	if body.Enabled != nil {
		dailyBonusAuto = *body.Enabled
	}
	cur := dailyBonusAuto
	dailyBonusAutoMu.Unlock()
	return map[string]any{"daily_bonus": cur, "persistent": false}
}
