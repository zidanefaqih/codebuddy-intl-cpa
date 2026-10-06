// retry.go implements the executor-side failover loop: when the account a
// request was routed to answers HTTP 429 / code 6004, the plugin retries the
// SAME request on another enabled account before returning an error to the
// client. The list of eligible accounts comes from the host auth store
// (hostAuthList + hostAuthGet — the same source the panel/usage use), not from
// a new parallel mechanism.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// failoverCandidate is one eligible account.
type failoverCandidate struct {
	// AuthID is the runtime auth_index used by host.auth.get.
	AuthID string
	// RecordID is the core auth.ID used by scheduler/panel selection.
	RecordID string
	// Aliases are other host-list identities, such as the physical filename.
	Aliases []string
	Auth    *storedAuth
}

// failoverPool is a single-request snapshot of the eligible accounts.
type failoverPool struct {
	Primary *failoverCandidate // account the host routed to
	Others  []*failoverCandidate
}

// wbAuthListCached caches host auth list results briefly so the retry loop
// doesn't hammer host RPCs on every 429 (refresh ≤30s).
var (
	authListMu    sync.Mutex
	authListCache []pluginapi.HostAuthFileEntry
	authListAt    time.Time
	// authListSource is the host list provider. Overridden by tests to avoid
	// host RPC; production code never reassigns it.
	authListSource = hostAuthList
	// authGetSource is the host credential provider. Overridden by tests.
	authGetSource = hostAuthGet
)

func loadAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	authListMu.Lock()
	defer authListMu.Unlock()
	if authListCache != nil && time.Since(authListAt) < 30*time.Second {
		return authListCache, nil
	}
	files, err := authListSource()
	if err != nil {
		return nil, err
	}
	authListCache = files
	authListAt = time.Now()
	return files, nil
}

// invalidateAuthListCache drops the cached host list so the next loadAuthList
// re-reads (called after auth enable/disable/delete changes).
func invalidateAuthListCache() {
	authListMu.Lock()
	authListCache = nil
	authListAt = time.Time{}
	authListMu.Unlock()
}

// listAuthEntryDisabled mirrors candidateDisabled for HostAuthFileEntry.
func listAuthEntryDisabled(f pluginapi.HostAuthFileEntry) bool {
	if f.Disabled {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(f.Status), "disabled")
}

// buildFailoverPool resolves the routed auth plus every other enabled
// workbuddy account the host knows about. Candidates flagged disabled or
// currently on rate-limit cooldown are excluded. When the host list is
// unavailable (tests / bridge down) the pool contains only the routed auth.
// primaryStorage is optional and preserves the routed realm when the host
// cannot resolve the routed record temporarily.
func buildFailoverPool(routedAuthID string, primaryStorage ...[]byte) *failoverPool {
	pool := &failoverPool{}
	files, err := loadAuthList()
	if err != nil {
		primary := &failoverCandidate{AuthID: routedAuthID, RecordID: routedAuthID}
		if len(primaryStorage) > 0 && len(primaryStorage[0]) > 0 {
			if sa, parseErr := parseStored(primaryStorage[0]); parseErr == nil {
				primary.Auth = sa
				primary.Aliases = []string{authFileNameFor(sa)}
			}
		}
		if !candidateOnCooldown(primary) {
			pool.Primary = primary
		}
		return pool
	}
	wantName := ""
	if routedAuthID != "" {
		wantName = "workbuddy-" + routedAuthID + ".json"
	}
	// A request is bound to one realm. Retrying a Global request with a CN
	// credential (or the reverse) cannot succeed and may expose a request to the
	// wrong account pool. Resolve the routed storage first and keep only peers
	// from the same CN/Global realm.
	var routedAuth *storedAuth
	routedMatched := false
	for _, f := range files {
		if f.AuthIndex == routedAuthID || f.ID == routedAuthID || f.Name == routedAuthID || listEntryMatchesUID(f, routedAuthID, wantName) {
			routedMatched = true
			if sa, getErr := authGetSource(f.AuthIndex); getErr == nil {
				routedAuth = sa
			}
			break
		}
	}
	if routedAuth == nil && routedAuthID != "" {
		if sa, getErr := authGetSource(routedAuthID); getErr == nil {
			routedAuth = sa
		}
	}
	if routedAuth == nil && len(primaryStorage) > 0 && len(primaryStorage[0]) > 0 {
		if sa, parseErr := parseStored(primaryStorage[0]); parseErr == nil {
			routedAuth = sa
		}
	}
	routedRealmKnown := routedAuth != nil
	routedGlobal := routedRealmKnown && isGlobalDomain(routedAuth.Auth.Domain)
	for _, f := range files {
		if listAuthEntryDisabled(f) || f.AuthIndex == "" {
			continue
		}
		if authEntryOnCooldown(f) {
			continue
		}
		cand := &failoverCandidate{AuthID: f.AuthIndex, RecordID: f.ID, Aliases: []string{f.Name}}
		if sa, err := authGetSource(f.AuthIndex); err == nil {
			cand.Auth = sa
		}
		isRouted := false
		if routedAuthID != "" {
			isRouted = f.AuthIndex == routedAuthID || f.ID == routedAuthID ||
				f.Name == routedAuthID || listEntryMatchesUID(f, routedAuthID, wantName)
		}
		if routedRealmKnown {
			if cand.Auth == nil {
				// The routed candidate can still use the executor's storage
				// snapshot, but an unresolved peer must not cross a known realm.
				if !isRouted {
					continue
				}
				cand.Auth = routedAuth
			} else if isGlobalDomain(cand.Auth.Auth.Domain) != routedGlobal {
				continue
			}
		}
		if isRouted {
			pool.Primary = cand
			continue
		}
		pool.Others = append(pool.Others, cand)
	}
	if pool.Primary == nil && !routedMatched {
		// Routed account missing from the list (just added / bridge edge) —
		// synthesize so at least the first attempt happens with the storage
		// the executor already carries. When the list did contain the account,
		// an excluded/disabled/cooldown primary must stay excluded.
		primary := &failoverCandidate{AuthID: routedAuthID, RecordID: routedAuthID}
		if len(primaryStorage) > 0 && len(primaryStorage[0]) > 0 {
			if sa, parseErr := parseStored(primaryStorage[0]); parseErr == nil {
				primary.Auth = sa
				primary.Aliases = []string{authFileNameFor(sa)}
			}
		}
		if !candidateOnCooldown(primary) {
			pool.Primary = primary
		}
	}
	return pool
}

// ordered dereferences the pool into attemptable candidates. The primary is
// always first (its storage fallback comes from the executor request), then
// each fallback with the credential the host returned. Candidates whose auth
// could not be resolved are skipped.
func (p *failoverPool) ordered(primaryStorage []byte) []*failoverCandidate {
	var out []*failoverCandidate
	seen := map[string]bool{}
	add := func(c *failoverCandidate, storage []byte) {
		if c == nil || c.AuthID == "" || seen[c.AuthID] {
			return
		}
		seen[c.AuthID] = true
		auth := c.Auth
		if auth == nil && len(storage) > 0 {
			if sa, err := parseStored(storage); err == nil {
				auth = sa
			}
		}
		out = append(out, &failoverCandidate{AuthID: c.AuthID, RecordID: c.RecordID, Aliases: append([]string(nil), c.Aliases...), Auth: auth})
	}
	add(p.Primary, primaryStorage)
	for _, c := range p.Others {
		add(c, nil)
	}
	return out
}

// upstreamAttempt performs ONE chat-completions round-trip (the upstream is
// always streamed; non-stream callers fold the SSE into a completion) and
// returns the folded completion payload. model is the fallback model name for
// the folded completion's "model" field.
//
// A 200 response that closes before any data chunk is treated as an error
// (errEmptyStream): the upstream accepted and billed the request but produced
// no output. Such attempts must NOT be retried on another account.
func upstreamAttempt(ctx context.Context, body []byte, model string, cand *failoverCandidate) ([]byte, error) {
	if cand == nil || cand.Auth == nil {
		return nil, fmt.Errorf("no auth available")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointChatFor(cand.Auth), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	backendHeaders(httpReq, cand.Auth)
	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	reader := newHostStreamReader(stream)
	if statusCode >= 400 {
		payload, _ := io.ReadAll(reader)
		return nil, &upstreamStatusError{status: statusCode, body: string(payload)}
	}
	completion, sawData, err := aggregateCompletionCounted(reader, model)
	if err != nil {
		return nil, err
	}
	if !sawData {
		return nil, errEmptyStream
	}
	return completion, nil
}

// upstreamStatusError carries an upstream non-2xx response through the
// failover loop so rate limits can be detected without parsing error strings.
type upstreamStatusError struct {
	status int
	body   string
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("upstream %d: %s", e.status, truncateRedacted(e.body, 200))
}

func upstreamStatusOf(err error) (int, string) {
	var ue *upstreamStatusError
	if errors.As(err, &ue) {
		return ue.status, ue.body
	}
	return 0, ""
}

// errEmptyStream marks an upstream that answered 200 but closed before any
// data chunk arrived. This is an UPSTREAM/request problem, not an account
// problem — retrying it on another account only double-bills credits (observed
// in production: both accounts charged for one failed request). It must stop
// the failover loop immediately and record failed=true.
var errEmptyStream = errors.New("empty_stream: upstream stream closed before first payload")

// isEmptyStreamError reports whether err is the plugin-side empty-stream
// marker (distinct from the host conductor's retryable empty_stream).
func isEmptyStreamError(err error) bool {
	return errors.Is(err, errEmptyStream)
}

// attemptOutcome is the classifier of one upstream attempt.
type attemptOutcome int

const (
	outcomeSuccess     attemptOutcome = iota // usable result
	outcomeRateLimited                       // soft 429 — try next account
	outcomeStop                              // transport/business error — do NOT retry
)

// failoverResult is the loop verdict.
type failoverResult struct {
	Outcome attemptOutcome
	AuthID  string // account that produced the result
	Tried   []string
	Emitted bool // stream: at least one chunk reached the client
	// For outcomeStop / final failure:
	Status int
	Body   string
	Err    error
}

// failoverAttemptFn performs one attempt against cand and classifies it.
type failoverAttemptFn func(cand *failoverCandidate) (attemptOutcome, int, string, error)

// runFailover executes attemptFn across eligible accounts: the routed account
// first, then each fallback. A rate-limited account is marked for cooldown
// and the next candidate is tried; any other failure stops the loop.
func runFailover(cands []*failoverCandidate, attemptFn failoverAttemptFn) failoverResult {
	res := failoverResult{Outcome: outcomeStop}
	for _, cand := range cands {
		if cand == nil || cand.Auth == nil || cand.AuthID == "" {
			continue
		}
		res.Tried = append(res.Tried, cand.AuthID)
		outcome, status, body, err := attemptFn(cand)
		switch outcome {
		case outcomeSuccess:
			resetCandidateCooldown(cand)
			return failoverResult{Outcome: outcomeSuccess, AuthID: cand.AuthID, Tried: res.Tried}
		case outcomeRateLimited:
			markCandidateCooldown(cand, parseRateLimitResetAt(body))
			res.Status, res.Body, res.Err = status, body, err
		default: // outcomeStop
			return failoverResult{Outcome: outcomeStop, AuthID: cand.AuthID, Tried: res.Tried,
				Status: status, Body: body, Err: err}
		}
	}
	// Every eligible account was rate-limited (or none were usable).
	res.Outcome = outcomeRateLimited
	if res.Err == nil {
		res.Err = &upstreamStatusError{status: 429, body: "all eligible workbuddy accounts are rate-limited"}
	}
	return res
}

// classifyNonStream classifies a folded (non-stream) attempt result.
func classifyNonStream(_ []byte, err error) (attemptOutcome, int, string, error) {
	if err == nil {
		return outcomeSuccess, 0, "", nil
	}
	status, body := upstreamStatusOf(err)
	if status == 0 {
		return outcomeStop, 0, "", err // transport error — account-independent
	}
	if isRateLimitResponse(status, body) {
		return outcomeRateLimited, status, body, err
	}
	return outcomeStop, status, body, err
}

// runStreamFailover drives streaming attempts across eligible accounts. Once
// the first chunk reached the client (emitted) or a non-rate-limit failure
// occurs, the loop stops immediately.
func runStreamFailover(cands []*failoverCandidate, attempt func(cand *failoverCandidate) (emitted bool, status int, err error)) failoverResult {
	res := failoverResult{Outcome: outcomeStop}
	for _, cand := range cands {
		if cand == nil || cand.Auth == nil || cand.AuthID == "" {
			continue
		}
		res.Tried = append(res.Tried, cand.AuthID)
		emitted, status, err := attempt(cand)
		if err == nil {
			resetCandidateCooldown(cand)
			return failoverResult{Outcome: outcomeSuccess, AuthID: cand.AuthID, Tried: res.Tried}
		}
		// Chunks already reached the client — the stream must end here.
		if emitted {
			return failoverResult{Outcome: outcomeStop, AuthID: cand.AuthID, Tried: res.Tried,
				Emitted: true, Status: status, Body: err.Error(), Err: err}
		}
		if isRateLimitResponse(status, err.Error()) {
			markCandidateCooldown(cand, parseRateLimitResetAt(err.Error()))
			res.Status, res.Body, res.Err = status, err.Error(), err
			continue
		}
		return failoverResult{Outcome: outcomeStop, AuthID: cand.AuthID, Tried: res.Tried,
			Status: status, Body: err.Error(), Err: err}
	}
	res.Outcome = outcomeRateLimited
	if res.Err == nil {
		res.Err = &upstreamStatusError{status: 429, body: "all eligible workbuddy accounts are rate-limited"}
	}
	return res
}

// candUID returns the account's UID for usage records; falls back to the
// routed account's UID when the candidate carries no parsed auth.
func markCandidateCooldown(c *failoverCandidate, resetAt time.Time) {
	for _, key := range candidateCooldownIDs(c) {
		markAuthCooldown(key, resetAt)
	}
}

func resetCandidateCooldown(c *failoverCandidate) {
	for _, key := range candidateCooldownIDs(c) {
		resetAuthCooldown(key)
	}
}

func candidateCooldownIDs(c *failoverCandidate) []string {
	if c == nil {
		return nil
	}
	keys := make([]string, 0, 3+len(c.Aliases))
	identities := []string{c.AuthID, c.RecordID}
	if c.Auth != nil {
		identities = append(identities, c.Auth.Account.UID)
	}
	identities = append(identities, c.Aliases...)
	seen := make(map[string]struct{}, len(identities))
	for _, key := range identities {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

func authEntryOnCooldown(f pluginapi.HostAuthFileEntry) bool {
	keys := []string{f.AuthIndex, f.ID, f.Name, authUIDFromFilename(f.Name)}
	for _, key := range keys {
		if isAuthOnCooldown(key) {
			return true
		}
	}
	return false
}

func authUIDFromFilename(name string) string {
	name = strings.TrimSpace(name)
	if !isWorkbuddyAuthName(name) || isLegacyWorkbuddyAuthName(name) {
		return ""
	}
	uid := name[len(providerName)+1:]
	if len(uid) < len(".json") || !strings.EqualFold(uid[len(uid)-len(".json"):], ".json") {
		return ""
	}
	return uid[:len(uid)-len(".json")]
}

func candidateOnCooldown(c *failoverCandidate) bool {
	for _, key := range candidateCooldownIDs(c) {
		if isAuthOnCooldown(key) {
			return true
		}
	}
	return false
}

func candUID(c *failoverCandidate, fallback string) string {
	if c != nil && c.Auth != nil && strings.TrimSpace(c.Auth.Account.UID) != "" {
		return c.Auth.Account.UID
	}
	return fallback
}

// findCandidate returns the pool candidate matching authID.
func findCandidate(cands []*failoverCandidate, authID string) *failoverCandidate {
	for _, c := range cands {
		if c != nil && c.AuthID == authID {
			return c
		}
	}
	return nil
}

func candRecordID(c *failoverCandidate, fallback string) string {
	if c != nil && strings.TrimSpace(c.RecordID) != "" {
		return c.RecordID
	}
	return fallback
}
