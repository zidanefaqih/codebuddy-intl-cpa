// request_usage.go implements the per-request CodeBuddy usage log ("Usage
// CodeBuddy" panel section): every chat request that flows through this
// plugin is recorded — account, model, token in/out/cache and the upstream
// `credit` value extracted from the terminal SSE chunk — into a permanent
// JSONL file next to the auth store.
//
// Source of truth is the JSONL file; an in-memory ring (recent records) serves
// fast panel renders and a day-bucket cache backs the summary totals. Storage
// is append-only JSONL (one record per line) so a crash can corrupt at most
// the trailing line. Rotation splits the file at ~50MB into
// request-usage-<YYYY-MM>.jsonl (older data is retained, never deleted).
//
// No credentials/tokens are ever stored — only usage metadata.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// requestUsageRecord is one JSONL line: metadata of a single request.
// Timestamp is RFC3339 UTC.
type requestUsageRecord struct {
	Timestamp        string  `json:"timestamp"`
	AuthIndex        string  `json:"auth_index"`
	AuthID           string  `json:"auth_id,omitempty"`
	Nickname         string  `json:"nickname,omitempty"`
	Region           string  `json:"region,omitempty"` // global | cn
	Model            string  `json:"model"`            // upstream model
	Alias            string  `json:"alias,omitempty"`  // requested alias when different
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`  // = hit
	CacheWriteTokens int64   `json:"cache_write_tokens"` // = write
	TotalTokens      int64   `json:"total_tokens"`
	Credit           float64 `json:"credit"` // upstream credit from final chunk (0 = free/under-threshold/length-cut; never estimated)
	Failed           bool    `json:"failed"`
	LatencyMS        int64   `json:"latency_ms"`
}

const (
	usageLogBaseName = "request-usage.jsonl"
	usageLogMaxBytes = 50 * 1024 * 1024
	usageRingSize    = 200
	usageLimitMax    = 1000
)

// dayAgg accumulates one day × account bucket in milli-credits (float drift).
type dayAgg struct {
	CreditMilli int64
	Requests    int64
}

type requestUsageStore struct {
	mu sync.Mutex

	dir  string // data dir, e.g. /root/.cli-proxy-api (host bind: ~/cliproxyapi/auths)
	path string // current JSONL file

	ring   []requestUsageRecord
	perDay map[string]map[string]*dayAgg // date(YYYY-MM-DD local) -> auth -> agg
	loaded bool
}

var usageStore = &requestUsageStore{perDay: map[string]map[string]*dayAgg{}}

// usageLogDir resolves the directory that holds the persistent JSONL.
//  1. env WORKBUDDY_DATA_DIR (explicit override)
//  2. dirname of the first auth file the host reports (the auth dir bind mount
//     — on this deployment /root/.cli-proxy-api ↔ /home/noel/cliproxyapi/auths)
//  3. $HOME (fallback; container default /root)
//  4. os.TempDir() (last resort; not persistent across restarts)
func usageLogDir() string {
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

// ensureLogDir locates (and lazily creates) the persistent log dir.
// Records live in a `.codebuddy_data` subdir of the auth/data dir so they
// stay out of the auth-file listing and survive restarts (host bind mount).
// WORKBUDDY_DATA_DIR overrides the location entirely (used as-is).
func (s *requestUsageStore) ensureLogDir() string {
	if s.path != "" {
		return s.path
	}
	base := usageLogDir()
	dir := base
	if strings.TrimSpace(os.Getenv("WORKBUDDY_DATA_DIR")) == "" {
		dir = filepath.Join(base, ".codebuddy_data")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	s.dir = dir
	s.path = filepath.Join(dir, usageLogBaseName)
	return s.path
}

// jsonlFiles returns current + rotated files, oldest first.
func (s *requestUsageStore) jsonlFiles() []string {
	matches, _ := filepath.Glob(filepath.Join(s.dir, "request-usage-*.jsonl"))
	files := make([]string, 0, len(matches)+1)
	if s.path != "" {
		files = append(files, s.path)
	}
	for _, m := range matches {
		if m != s.path {
			files = append(files, m)
		}
	}
	sort.Strings(files)
	return files
}

// bucketAdd folds one record into the per-day × per-account cache.
// Caller must hold s.mu.
func (s *requestUsageStore) bucketAdd(rec requestUsageRecord) {
	ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return
	}
	key := ts.Local().Format("2006-01-02")
	auth := rec.AuthIndex
	if auth == "" {
		auth = rec.AuthID
	}
	if auth == "" {
		auth = "_unknown"
	}
	m := s.perDay[key]
	if m == nil {
		m = map[string]*dayAgg{}
		s.perDay[key] = m
	}
	a := m[auth]
	if a == nil {
		a = &dayAgg{}
		m[auth] = a
	}
	a.CreditMilli += int64(rec.Credit*1000 + 0.5)
	if !rec.Failed {
		a.Requests++
	}
}

// loadFromDisk rebuilds the day-bucket cache (full scan) and fills the ring
// with the newest records. Called lazily before the first management query so
// a fresh process still shows persisted history.
func (s *requestUsageStore) loadFromDisk() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return
	}
	if s.dir == "" {
		s.ensureLogDir()
	}
	s.perDay = map[string]map[string]*dayAgg{}
	var newest []requestUsageRecord
	for _, f := range s.jsonlFiles() {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec requestUsageRecord
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue // tolerate a corrupt trailing line
			}
			s.bucketAdd(rec)
			newest = append(newest, rec)
		}
	}
	if len(newest) > usageRingSize {
		newest = newest[len(newest)-usageRingSize:]
	}
	s.ring = newest
	s.loaded = true
}

// record appends one usage line: JSONL append (single O_APPEND write) +
// in-memory ring + day buckets. Never returns errors — chat path unaffected.
func (s *requestUsageStore) record(rec requestUsageRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bucketAdd(rec)
	s.ring = append(s.ring, rec)
	if len(s.ring) > usageRingSize {
		s.ring = s.ring[len(s.ring)-usageRingSize:]
	}
	path := s.ensureLogDir()
	if path == "" {
		return
	}
	// Rotate when the current file exceeds the cap (keep the old file).
	ts, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if fi, err := os.Stat(path); err == nil && fi.Size() > usageLogMaxBytes {
		rot := filepath.Join(s.dir, "request-usage-"+ts.Local().Format("2006-01")+".jsonl")
		if rot != path {
			_ = os.Rename(path, rot)
		}
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// queryRecords filters by auth_index/from/to, newest first, up to limit.
// The ring serves the default no-filter view; date/account ranges fall back
// to a disk scan so yesterday/7d history survives ring eviction and restarts.
func (s *requestUsageStore) queryRecords(authIndex, from, to string, limit int) []requestUsageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	fromT, toT := parseRange(from, to)
	// Ring shortcut: no filter and the ring already covers the limit.
	if authIndex == "" && fromT.IsZero() && toT.IsZero() && len(s.ring) >= limit {
		out := make([]requestUsageRecord, 0, limit)
		for i := len(s.ring) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, s.ring[i])
		}
		return out
	}
	// Disk scan, newest first: files newest→oldest, lines newest→oldest.
	var out []requestUsageRecord
	files := s.jsonlFiles()
scan:
	for fi := len(files) - 1; fi >= 0 && len(out) < limit; fi-- {
		raw, err := os.ReadFile(files[fi])
		if err != nil {
			continue
		}
		lines := strings.Split(string(raw), "\n")
		for li := len(lines) - 1; li >= 0 && len(out) < limit; li-- {
			line := strings.TrimSpace(lines[li])
			if line == "" {
				continue
			}
			var rec requestUsageRecord
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			if authIndex != "" && rec.AuthIndex != authIndex && rec.AuthID != authIndex {
				continue
			}
			ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
			if err != nil {
				continue
			}
			local := ts.Local()
			if !toT.IsZero() && local.After(toT) {
				continue
			}
			if !fromT.IsZero() {
				if local.Before(fromT) {
					// Lines only get older from here — stop this file, and if
					// the newest line of this file is already older than from,
					// older files can't match either.
					if li == len(lines)-1 {
						break scan
					}
					continue
				}
			}
			out = append(out, rec)
		}
	}
	return out
}

// parseRange converts from/to (RFC3339 or YYYY-MM-DD local) to instants.
// Date-only values become local midnight (from) and end-of-day (to).
func parseRange(from, to string) (fromT, toT time.Time) {
	if f := strings.TrimSpace(from); f != "" {
		if t, err := parseFlexTime(f); err == nil {
			fromT = t
		}
	}
	if t := strings.TrimSpace(to); t != "" {
		if v, err := parseFlexTime(t); err == nil {
			toT = v
			if !strings.Contains(t, "T") {
				toT = v.Add(24*time.Hour - time.Second) // inclusive end-of-day
			}
		}
	}
	return
}

// parseFlexTime accepts RFC3339(Nano) or YYYY-MM-DD (local midnight).
func parseFlexTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, errors.New("empty time")
	}
	if strings.Contains(v, "T") {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, v); err == nil {
				return t, nil
			}
		}
		return time.Time{}, errors.New("bad time")
	}
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("bad time")
}

// dayTotals returns (credit, requests) for one local day key.
func (s *requestUsageStore) dayTotals(day string) (float64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.perDay[day]
	if m == nil {
		return 0, 0
	}
	var milli, req int64
	for _, a := range m {
		milli += a.CreditMilli
		req += a.Requests
	}
	return float64(milli) / 1000.0, req
}

// rangeTotals sums day buckets whose local date falls inside [fromT, toT]
// (day granularity — good enough for header summaries; the records list
// itself is filtered with exact timestamps). Zero from/to = unbounded.
func (s *requestUsageStore) rangeTotals(fromT, toT time.Time, authIndex string) (float64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var milli, req int64
	for day, m := range s.perDay {
		t, err := time.Parse("2006-01-02", day)
		if err != nil {
			continue
		}
		if !fromT.IsZero() && t.Before(fromT) {
			continue
		}
		if !toT.IsZero() && t.After(toT) {
			continue
		}
		for auth, a := range m {
			if authIndex != "" && auth != authIndex && auth != "_unknown" {
				continue
			}
			milli += a.CreditMilli
			req += a.Requests
		}
	}
	return float64(milli) / 1000.0, req
}

// allTimeTotals sums every loaded day bucket.
func (s *requestUsageStore) allTimeTotals() (float64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var milli, req int64
	for _, m := range s.perDay {
		for _, a := range m {
			milli += a.CreditMilli
			req += a.Requests
		}
	}
	return float64(milli) / 1000.0, req
}

// accountDayTotals returns per-account totals for one local day key.
func (s *requestUsageStore) accountDayTotals(day string) map[string]dayAgg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]dayAgg{}
	for auth, a := range s.perDay[day] {
		out[auth] = *a
	}
	return out
}

// accountAllTotals returns per-account all-time totals.
func (s *requestUsageStore) accountAllTotals() map[string]dayAgg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]dayAgg{}
	for _, m := range s.perDay {
		for auth, a := range m {
			agg := out[auth]
			agg.CreditMilli += a.CreditMilli
			agg.Requests += a.Requests
			out[auth] = agg
		}
	}
	return out
}

// ringSnapshot returns a copy of the recent-record ring (newest last).
func (s *requestUsageStore) ringSnapshot() []requestUsageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]requestUsageRecord, len(s.ring))
	copy(out, s.ring)
	return out
}

// -----------------------------------------------------------------------------
// Credit extraction + executor hooks
// -----------------------------------------------------------------------------

// creditFromUsageMap reads the upstream "credit" field (float64 / json.Number
// / int). credit:0 means free / under-threshold / finish_reason=length — never
// estimate it from tokens.
func creditFromUsageMap(m map[string]any) float64 {
	if len(m) == 0 {
		return 0
	}
	v, ok := m["credit"]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

// credit returns the credit of the last usage chunk seen by the collector.
func (c *sseUsageCollector) credit() float64 {
	return creditFromUsageMap(c.last)
}

// lastMap exposes the collector's last usage chunk (record hook input).
func (c *sseUsageCollector) lastMap() map[string]any {
	if c == nil {
		return nil
	}
	return c.last
}

// usageMapFromCompletion extracts the "usage" object of a folded completion.
func usageMapFromCompletion(payload []byte) map[string]any {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return nil
	}
	m, _ := obj["usage"].(map[string]any)
	return m
}

// usagePartsFromMap extracts tokens (incl. cache write) + credit in one pass.
// usage.go / publishUsage stay untouched — this is record-only.
func usagePartsFromMap(m map[string]any) (usage.Detail, float64) {
	if len(m) == 0 {
		return usage.Detail{}, 0
	}
	num := func(keys ...string) int64 {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				switch n := v.(type) {
				case float64:
					return int64(n)
				case int64:
					return n
				case json.Number:
					i, _ := n.Int64()
					return i
				}
			}
		}
		return 0
	}
	d := usage.Detail{
		InputTokens:         num("prompt_tokens", "input_tokens"),
		OutputTokens:        num("completion_tokens", "output_tokens"),
		TotalTokens:         num("total_tokens"),
		CachedTokens:        num("cached_tokens"),
		CacheReadTokens:     num("prompt_cache_hit_tokens", "cache_read_input_tokens"),
		CacheCreationTokens: num("prompt_cache_write_tokens", "cache_creation_tokens"),
	}
	if ct, ok := m["completion_tokens_details"].(map[string]any); ok {
		if v, ok2 := ct["reasoning_tokens"].(float64); ok2 {
			d.ReasoningTokens = int64(v)
		}
	}
	if d.TotalTokens == 0 {
		d.TotalTokens = d.InputTokens + d.OutputTokens + d.ReasoningTokens
	}
	return d, creditFromUsageMap(m)
}

// usageAuthMeta is the cached per-account display metadata.
type usageAuthMeta struct {
	AuthIndex string
	Nickname  string
	Region    string
}

// usageMetaResolver caches auth_index/nickname/region per auth ID so the
// record hook doesn't call host RPCs on every chat request (refresh ≤60s).
type usageMetaResolver struct {
	mu        sync.Mutex
	byAuthID  map[string]usageAuthMeta
	byUID     map[string]usageAuthMeta
	byIndex   map[string]usageAuthMeta
	refreshed time.Time
}

var usageMetaCache usageMetaResolver

func (r *usageMetaResolver) refresh() {
	now := time.Now()
	if r.byAuthID != nil && now.Sub(r.refreshed) < 60*time.Second {
		return
	}
	byAuthID := map[string]usageAuthMeta{}
	byUID := map[string]usageAuthMeta{}
	byIndex := map[string]usageAuthMeta{}
	if files, err := hostAuthList(); err == nil {
		for _, f := range files {
			meta := usageAuthMeta{AuthIndex: f.AuthIndex}
			if sa, err := hostAuthGet(f.AuthIndex); err == nil {
				meta.Nickname = sa.Account.Nickname
				meta.Region = accountRegion(sa)
				byUID[sa.Account.UID] = meta
			}
			if f.ID != "" {
				byAuthID[f.ID] = meta
			}
			if meta.AuthIndex != "" {
				byIndex[meta.AuthIndex] = meta
			}
		}
	}
	r.byAuthID, r.byUID, r.byIndex = byAuthID, byUID, byIndex
	r.refreshed = now
}

// resolve maps authID/authUID to display metadata (cached).
func (r *usageMetaResolver) resolve(authID, authUID string) usageAuthMeta {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh()
	if authID != "" {
		if m, ok := r.byAuthID[authID]; ok {
			return m
		}
	}
	if authUID != "" {
		if m, ok := r.byUID[authUID]; ok {
			return m
		}
		if m, ok := r.byIndex[authUID]; ok {
			return m
		}
	}
	return usageAuthMeta{}
}

// buildUsageRecord assembles one record from executor call-site data.
func buildUsageRecord(requestedModel, upstreamModel, authID, authUID string, started time.Time, detail usage.Detail, credit float64, failed bool) requestUsageRecord {
	model := strings.TrimSpace(upstreamModel)
	if model == "" {
		model = strings.TrimSpace(requestedModel)
	}
	alias := strings.TrimSpace(requestedModel)
	if alias == model {
		alias = ""
	}
	meta := usageMetaCache.resolve(authID, authUID)
	region := meta.Region
	if region == "" {
		region = "global"
	}
	total := detail.TotalTokens
	if total == 0 {
		total = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	latency := int64(0)
	if !started.IsZero() {
		latency = time.Since(started).Milliseconds()
		if latency < 0 {
			latency = 0
		}
	}
	return requestUsageRecord{
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
		AuthIndex:        firstNonEmpty(meta.AuthIndex, authUID, authID),
		AuthID:           authID,
		Nickname:         meta.Nickname,
		Region:           region,
		Model:            model,
		Alias:            alias,
		InputTokens:      detail.InputTokens,
		OutputTokens:     detail.OutputTokens,
		ReasoningTokens:  detail.ReasoningTokens,
		CacheReadTokens:  detail.CacheReadTokens,
		CacheWriteTokens: detail.CacheCreationTokens,
		TotalTokens:      total,
		Credit:           credit,
		Failed:           failed,
		LatencyMS:        latency,
	}
}

// recordUsageMap is the single hook for executor paths that observed the
// upstream usage chunk map (stream collectors, folded completions).
func recordUsageMap(requestedModel, upstreamModel, authID, authUID string, started time.Time, m map[string]any, failed bool) {
	if len(m) == 0 && !failed {
		return // no usage signal at all and no failure — nothing worth logging
	}
	detail, credit := usagePartsFromMap(m)
	usageStore.record(buildUsageRecord(requestedModel, upstreamModel, authID, authUID, started, detail, credit, failed))
}

// -----------------------------------------------------------------------------
// Management endpoints (read-only; auth = same as existing /credits reads)
// -----------------------------------------------------------------------------

// handleUsageQuery implements GET /usage?auth_index=&limit=&from=&to=.
func handleUsageQuery(req pluginapi.ManagementRequest) map[string]any {
	limit := 100
	if vals := req.Query["limit"]; len(vals) > 0 {
		if n, err := fmt.Sscanf(strings.TrimSpace(vals[0]), "%d", &limit); err == nil && n == 1 {
			if limit < 1 {
				limit = 1
			}
			if limit > usageLimitMax {
				limit = usageLimitMax
			}
		} else {
			limit = 100
		}
	}
	authIndex := ""
	if vals := req.Query["auth_index"]; len(vals) > 0 {
		authIndex = strings.TrimSpace(vals[0])
	}
	from, to := "", ""
	if vals := req.Query["from"]; len(vals) > 0 {
		from = strings.TrimSpace(vals[0])
	}
	if vals := req.Query["to"]; len(vals) > 0 {
		to = strings.TrimSpace(vals[0])
	}
	usageStore.loadFromDisk()
	records := usageStore.queryRecords(authIndex, from, to, limit)
	if records == nil {
		records = []requestUsageRecord{}
	}
	fromT, toT := parseRange(from, to)
	rangeCredit, rangeReq := usageStore.rangeTotals(fromT, toT, authIndex)
	todayCredit, todayReq := usageStore.dayTotals(time.Now().Format("2006-01-02"))
	totalCredit, totalReq := usageStore.allTimeTotals()
	return map[string]any{
		"records":       records,
		"total_records": len(records),
		"totals": map[string]any{
			"today_credit":   todayCredit,
			"today_requests": todayReq,
			"total_credit":   totalCredit,
			"total_requests": totalReq,
			"range_credit":   rangeCredit,
			"range_requests": rangeReq,
		},
	}
}

// handleUsageSummary implements GET /usage/summary — per-account quick stats.
func handleUsageSummary(req pluginapi.ManagementRequest) map[string]any {
	usageStore.loadFromDisk()
	today := time.Now().Format("2006-01-02")
	type row struct {
		Nickname      string
		Region        string
		LastTimestamp string
		LastCredit    float64
		TodayCredit   float64
		TodayRequests int64
		TotalCredit   float64
		TotalRequests int64
	}
	rows := map[string]*row{}
	order := []string{}
	merge := func(auth string, credit float64, reqN int64, todayBucket bool) {
		r := rows[auth]
		if r == nil {
			r = &row{}
			rows[auth] = r
			order = append(order, auth)
		}
		if todayBucket {
			r.TodayCredit += credit
			r.TodayRequests += reqN
		} else {
			r.TotalCredit += credit
			r.TotalRequests += reqN
		}
	}
	for auth, agg := range usageStore.accountAllTotals() {
		merge(auth, float64(agg.CreditMilli)/1000.0, agg.Requests, false)
	}
	for auth, agg := range usageStore.accountDayTotals(today) {
		merge(auth, float64(agg.CreditMilli)/1000.0, agg.Requests, true)
	}
	// "Last request" per account from the ring.
	lastByAuth := map[string]requestUsageRecord{}
	for _, rec := range usageStore.ringSnapshot() {
		key := firstNonEmpty(rec.AuthIndex, rec.AuthID)
		if key == "" {
			continue
		}
		cur, seen := lastByAuth[key]
		if !seen || rec.Timestamp > cur.Timestamp {
			lastByAuth[key] = rec
		}
	}
	// Enrich display names (cache resolver may be warm from record hooks).
	metaByAuth := map[string]usageAuthMeta{}
	for _, auth := range order {
		m := usageMetaCache.resolve("", auth)
		if m.AuthIndex == "" {
			m = usageMetaCache.resolve(auth, "")
		}
		metaByAuth[auth] = m
	}
	out := make([]map[string]any, 0, len(order))
	for _, auth := range order {
		r := rows[auth]
		meta := metaByAuth[auth]
		last := lastByAuth[auth]
		region := meta.Region
		if region == "" {
			region = "global"
		}
		out = append(out, map[string]any{
			"auth_index":     auth,
			"nickname":       firstNonEmpty(meta.Nickname, auth),
			"region":         region,
			"last":           last.Timestamp,
			"last_credit":    last.Credit,
			"today_credit":   r.TodayCredit,
			"today_requests": r.TodayRequests,
			"total_credit":   r.TotalCredit,
			"total_requests": r.TotalRequests,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["today_credit"].(float64) > out[j]["today_credit"].(float64)
	})
	return map[string]any{"accounts": out}
}
