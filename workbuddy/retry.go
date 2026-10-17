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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
	// RateModel scopes soft rate limits to the actual upstream model. Credit
	// exhaustion remains account-wide. RecoveryProbe is only set by the pool
	// when every eligible account is cooling; admission still enforces backoff.
	RateModel     string
	RecoveryProbe bool
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

func isCNAuth(sa *storedAuth) bool {
	return sa != nil && !isGlobalDomain(sa.Auth.Domain) && !isGlobalToken(sa.Auth.AccessToken)
}

// buildFailoverPool resolves the routed auth plus every other enabled
// workbuddy account the host knows about. Candidates flagged disabled or
// currently on rate-limit cooldown are excluded. When the host list is
// unavailable (tests / bridge down) the pool contains only the routed auth.
// primaryStorage is optional and preserves the routed realm when the host
// cannot resolve the routed record temporarily.
func buildFailoverPool(routedAuthID string, primaryStorage ...[]byte) *failoverPool {
	return buildBareFailoverPool(routedAuthID, "", primaryStorage...)
}

func buildBareFailoverPool(routedAuthID, model string, primaryStorage ...[]byte) *failoverPool {
	pool := &failoverPool{}
	var cooling []*failoverCandidate
	files, err := loadAuthList()
	if err != nil {
		primary := &failoverCandidate{AuthID: routedAuthID, RecordID: routedAuthID}
		if len(primaryStorage) > 0 && len(primaryStorage[0]) > 0 {
			if sa, parseErr := parseStored(primaryStorage[0]); parseErr == nil {
				primary.Auth = sa
				primary.Aliases = []string{authFileNameFor(sa)}
			}
		}
		primary.RateModel = candidateRateModel(model, primary.Auth)
		if !candidateOnCooldown(primary) || candidateSoftCooldown(primary) {
			primary.RecoveryProbe = candidateOnCooldown(primary)
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
		cand.RateModel = candidateRateModel(model, cand.Auth)
		if candidateOnCooldown(cand) {
			if candidateSoftCooldown(cand) {
				if isRouted {
					cooling = append([]*failoverCandidate{cand}, cooling...)
				} else {
					cooling = append(cooling, cand)
				}
			}
			continue
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
		primary.RateModel = candidateRateModel(model, primary.Auth)
		if !candidateOnCooldown(primary) {
			pool.Primary = primary
		}
	}
	if model != "" && pool.Primary == nil && len(pool.Others) == 0 && len(cooling) > 0 && routedRealmKnown {
		pick := cooling[0]
		if !candidateProbeReady(pick) {
			for _, c := range cooling[1:] {
				if candidateProbeAt(c).Before(candidateProbeAt(pick)) {
					pick = c
				}
			}
		}
		pick.RecoveryProbe = true
		pool.Primary = pick
	}
	return pool
}

// buildRequestFailoverPool routes a prefixed model to a realm-only candidate
// pool: a CN-prefixed id gets CN-only candidates, a Global-prefixed id gets
// Global-only candidates, so neither the routed account nor a later 429 retry
// can send that request to the other realm (and the executor strips the model
// prefix once, against the executing account's realm). Bare model IDs keep the
// legacy realm-aware route.
func buildRequestFailoverPool(model, routedAuthID string, routedStorage []byte, attributes ...map[string]string) (*failoverPool, []byte, error) {
	realm := mmPrefixRealm(model)
	rateModel := model
	var attrs map[string]string
	if len(attributes) > 0 {
		attrs = attributes[0]
	}
	hint := realm
	if hint == "" {
		if auth, err := parseStored(routedStorage); err == nil {
			hint = accountRegion(auth)
		}
	}
	rateModel = resolveUpstreamModel(model, hint, attrs)
	if realm == "" {
		return buildBareFailoverPool(routedAuthID, rateModel, routedStorage), routedStorage, nil
	}
	return buildRealmFailoverPool(rateModel, routedAuthID, routedStorage, realm)
}

// authInRealm reports whether a credential belongs to the given realm.
func authInRealm(sa *storedAuth, realm string) bool {
	if sa == nil {
		return false
	}
	if realm == realmGlobal {
		return !isCNAuth(sa)
	}
	return isCNAuth(sa)
}

func realmLabel(realm string) string {
	if realm == realmGlobal {
		return "Global"
	}
	return "CN"
}

// buildRealmFailoverPool builds the candidate pool for one realm only.
func buildRealmFailoverPool(model, routedAuthID string, routedStorage []byte, realm string) (*failoverPool, []byte, error) {
	var routedAuth *storedAuth
	if len(routedStorage) > 0 {
		routedAuth, _ = parseStored(routedStorage)
	}
	files, err := loadAuthList()
	if err != nil {
		cand := &failoverCandidate{AuthID: routedAuthID, RecordID: routedAuthID, Auth: routedAuth, RateModel: candidateRateModel(model, routedAuth)}
		if authInRealm(routedAuth, realm) {
			cand.RecoveryProbe = candidateSoftCooldown(cand)
			return &failoverPool{Primary: cand}, routedStorage, nil
		}
		return nil, nil, fmt.Errorf("cannot find a %s CodeBuddy account for model %q: %w", realmLabel(realm), model, err)
	}

	type eligible struct {
		file pluginapi.HostAuthFileEntry
		cand *failoverCandidate
	}
	routedName := ""
	if routedAuthID != "" {
		routedName = "workbuddy-" + routedAuthID + ".json"
	}
	matchesRouted := func(f pluginapi.HostAuthFileEntry) bool {
		return routedAuthID != "" && (f.AuthIndex == routedAuthID || f.ID == routedAuthID ||
			f.Name == routedAuthID || listEntryMatchesUID(f, routedAuthID, routedName))
	}
	var candidates, cooling []eligible
	for _, f := range files {
		if listAuthEntryDisabled(f) || f.AuthIndex == "" {
			continue
		}
		sa, getErr := authGetSource(f.AuthIndex)
		if getErr != nil || !authInRealm(sa, realm) {
			continue
		}
		cand := &failoverCandidate{AuthID: f.AuthIndex, RecordID: f.ID, Aliases: []string{f.Name}, Auth: sa, RateModel: candidateRateModel(model, sa)}
		if candidateOnCooldown(cand) {
			cooling = append(cooling, eligible{file: f, cand: cand})
			continue
		}
		candidates = append(candidates, eligible{file: f, cand: cand})
	}
	if len(candidates) == 0 {
		// Admit at most one bounded recovery probe, and only for a soft rate
		// limit. Exhausted accounts never become probes. Prefer the routed
		// ready probe; otherwise rank by the effective expiry of ALL aliases.
		var pick *failoverCandidate
		for _, c := range cooling {
			if candidateSoftCooldown(c.cand) && matchesRouted(c.file) && candidateProbeReady(c.cand) {
				pick = c.cand
				break
			}
		}
		if pick == nil {
			for _, c := range cooling {
				if candidateSoftCooldown(c.cand) && (pick == nil || candidateProbeAt(c.cand).Before(candidateProbeAt(pick))) {
					pick = c.cand
				}
			}
		}
		if pick != nil {
			pick.RecoveryProbe = true
		} else if len(cooling) > 0 {
			// Keep valid storage/identity for terminal accounting, not an
			// executable probe. The loop will return unavailable without I/O.
			pick = cooling[0].cand
		}
		if pick == nil {
			return nil, nil, fmt.Errorf("no enabled %s CodeBuddy account is available for model %q", realmLabel(realm), model)
		}
		storage, err := json.Marshal(pick.Auth)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot prepare %s account: %w", realmLabel(realm), err)
		}
		return &failoverPool{Primary: pick}, storage, nil
	}

	// Primary is the account the host routed to when it is still eligible,
	// otherwise the first eligible candidate. There is deliberately no
	// panel-selected global account here: one global choice leaked across
	// sessions and models, so a failover in one conversation moved another
	// conversation onto an unrelated account and re-wrote its prompt cache.
	selected := 0
	for i, c := range candidates {
		if matchesRouted(c.file) {
			selected = i
			break
		}
	}

	pool := &failoverPool{Primary: candidates[selected].cand}
	for i, c := range candidates {
		if i != selected {
			pool.Others = append(pool.Others, c.cand)
		}
	}
	storage, err := json.Marshal(pool.Primary.Auth)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot prepare selected %s account: %w", realmLabel(realm), err)
	}
	return pool, storage, nil
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
		out = append(out, &failoverCandidate{AuthID: c.AuthID, RecordID: c.RecordID, Aliases: append([]string(nil), c.Aliases...), Auth: auth, RateModel: c.RateModel, RecoveryProbe: c.RecoveryProbe})
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
func upstreamAttempt(ctx context.Context, body []byte, model string, cand *failoverCandidate, traces ...*requestCacheTrace) ([]byte, error) {
	if cand == nil || cand.Auth == nil {
		return nil, fmt.Errorf("no auth available")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointChatFor(cand.Auth), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	backendHeaders(httpReq, cand.Auth)
	trace := firstCacheTrace(traces)
	attempt := trace.beginAttempt(httpReq, cand.AuthID)
	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	trace.finishAttempt(attempt, statusCode)
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

func (e *upstreamStatusError) StatusCode() int {
	return e.status
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
	outcomeSuccess          attemptOutcome = iota // usable result
	outcomeRateLimited                            // soft 429 — try next account
	outcomeAccountExhausted                       // explicit credit rejection — skip + try next
	outcomeStop                                   // transport/business error — do NOT retry
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
// first, then each fallback. Throttled/exhausted accounts are temporarily
// skipped (no auth mutation); ambiguous or request-wide failures stop the loop.
func runFailover(cands []*failoverCandidate, attemptFn failoverAttemptFn, contexts ...context.Context) failoverResult {
	return runFailoverForSession(cands, "", attemptFn, contexts...)
}

func runFailoverForSession(cands []*failoverCandidate, sessionKey string, attemptFn failoverAttemptFn, contexts ...context.Context) failoverResult {
	res := failoverResult{Outcome: outcomeStop}
	busy := false
	for _, cand := range chatSessions.preferred(sessionKey, cands) {
		release, admissionErr := admitCandidateAttempt(attemptContext(contexts), cand)
		if errors.Is(admissionErr, errAccountBusy) {
			busy = true
			continue // try a free same-realm account, without local waiting
		}
		if admissionErr != nil {
			return admissionFailureResult(res, admissionErr)
		}
		if release == nil {
			continue
		}
		res.Tried = append(res.Tried, cand.AuthID)
		outcome, status, body, err := func() (outcome attemptOutcome, status int, body string, err error) {
			defer release()
			binding := chatSessions.begin(sessionKey, accountGateKey(cand))
			defer func() { chatSessions.finish(sessionKey, binding, outcome == outcomeSuccess) }()
			outcome, status, body, err = attemptFn(cand)
			// Publish quarantine before another request can take the freed slot.
			if outcome == outcomeRateLimited || outcome == outcomeAccountExhausted {
				markCandidateUnavailable(cand, outcome, body)
			} else if outcome == outcomeSuccess {
				resetCandidateCooldown(cand)
			}
			return outcome, status, body, err
		}()
		switch outcome {
		case outcomeSuccess:
			return failoverResult{Outcome: outcomeSuccess, AuthID: cand.AuthID, Tried: res.Tried}
		case outcomeRateLimited, outcomeAccountExhausted:
			res.Outcome, res.AuthID = outcome, cand.AuthID
			res.Status, res.Body, res.Err = status, body, err
		default: // outcomeStop
			return failoverResult{Outcome: outcomeStop, AuthID: cand.AuthID, Tried: res.Tried,
				Status: status, Body: body, Err: err}
		}
	}
	return unavailableFailoverResult(res, busy)
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
	if isAccountExhaustedResponse(status, body) {
		return outcomeAccountExhausted, status, body, err
	}
	if isRateLimitResponse(status, body) {
		return outcomeRateLimited, status, body, err
	}
	return outcomeStop, status, body, err
}

// runStreamFailover drives streaming attempts across eligible accounts. Once
// the first chunk reached the client (emitted) or a non-rate-limit failure
// occurs, the loop stops immediately.
func runStreamFailover(cands []*failoverCandidate, attempt func(cand *failoverCandidate) (emitted bool, status int, err error), contexts ...context.Context) failoverResult {
	return runStreamFailoverForSession(cands, "", attempt, contexts...)
}

func runStreamFailoverForSession(cands []*failoverCandidate, sessionKey string, attempt func(cand *failoverCandidate) (emitted bool, status int, err error), contexts ...context.Context) failoverResult {
	res := failoverResult{Outcome: outcomeStop}
	busy := false
	for _, cand := range chatSessions.preferred(sessionKey, cands) {
		release, admissionErr := admitCandidateAttempt(attemptContext(contexts), cand)
		if errors.Is(admissionErr, errAccountBusy) {
			busy = true
			continue
		}
		if admissionErr != nil {
			return admissionFailureResult(res, admissionErr)
		}
		if release == nil {
			continue
		}
		res.Tried = append(res.Tried, cand.AuthID)
		emitted, status, err := func() (emitted bool, status int, err error) {
			defer release()
			binding := chatSessions.begin(sessionKey, accountGateKey(cand))
			defer func() { chatSessions.finish(sessionKey, binding, err == nil || emitted) }()
			emitted, status, err = attempt(cand)
			if err == nil {
				resetCandidateCooldown(cand)
			} else if !emitted {
				if isAccountExhaustedResponse(status, err.Error()) {
					markCandidateUnavailable(cand, outcomeAccountExhausted, err.Error())
				} else if isRateLimitResponse(status, err.Error()) {
					markCandidateUnavailable(cand, outcomeRateLimited, err.Error())
				}
			}
			return emitted, status, err
		}()
		if err == nil {
			return failoverResult{Outcome: outcomeSuccess, AuthID: cand.AuthID, Tried: res.Tried}
		}
		// Chunks already reached the client — the stream must end here.
		if emitted {
			return failoverResult{Outcome: outcomeStop, AuthID: cand.AuthID, Tried: res.Tried,
				Emitted: true, Status: status, Body: err.Error(), Err: err}
		}
		outcome := outcomeStop
		if isAccountExhaustedResponse(status, err.Error()) {
			outcome = outcomeAccountExhausted
		} else if isRateLimitResponse(status, err.Error()) {
			outcome = outcomeRateLimited
		}
		if outcome != outcomeStop {
			res.Outcome, res.AuthID = outcome, cand.AuthID
			res.Status, res.Body, res.Err = status, err.Error(), err
			continue
		}
		return failoverResult{Outcome: outcomeStop, AuthID: cand.AuthID, Tried: res.Tried,
			Status: status, Body: err.Error(), Err: err}
	}
	return unavailableFailoverResult(res, busy)
}

func admissionFailureResult(res failoverResult, err error) failoverResult {
	status, body := upstreamStatusOf(err)
	outcome := outcomeStop
	if status == 429 {
		outcome = outcomeRateLimited
	} // local overload: no lifecycle/account mutation
	if body == "" {
		body = err.Error()
	}
	return failoverResult{Outcome: outcome, AuthID: res.AuthID, Tried: res.Tried, Status: status, Body: body, Err: err}
}

func unavailableFailoverResult(res failoverResult, busy ...bool) failoverResult {
	if res.Err == nil {
		res.Outcome = outcomeRateLimited
		res.Status, res.Body = 429, "all eligible workbuddy accounts are temporarily unavailable"
		if len(busy) > 0 && busy[0] {
			res.Body = "all eligible workbuddy accounts are busy; retry later"
		}
		res.Err = &upstreamStatusError{status: res.Status, body: res.Body}
	}
	return res
}

// Credit rejection uses the existing 30-minute backoff when no reset is
// provided. This is process-local, independent of lifecycle_auto, and never
// disables/deletes an auth file; replenished accounts can be retried later.
func markCandidateUnavailable(c *failoverCandidate, outcome attemptOutcome, body string) {
	if c == nil {
		return
	}
	reason := "429"
	if outcome == outcomeAccountExhausted {
		reason = "credits_exhausted"
	}
	resetAt := parseRateLimitResetAt(body)
	if resetAt.IsZero() || !resetAt.After(time.Now()) {
		resetAt = time.Now().Add(defaultCooldownDuration)
	}
	if reason == "429" {
		// Soft rate limits recover; never let a burst freeze the whole realm
		// for the full 30-minute window.
		if capAt := time.Now().Add(maxRateLimitCooldown); resetAt.After(capAt) {
			resetAt = capAt
		}
	}
	for _, key := range candidateCooldownIDs(c) {
		if reason == "429" && c.RateModel != "" {
			key = modelCooldownKey(key, c.RateModel)
		}
		markAuthCooldownReason(key, resetAt, reason)
	}
	fmt.Fprintf(os.Stderr, "[workbuddy] failover: skip auth=%q model=%q reason=%s until=%s\n", redactSecrets(c.AuthID), redactSecrets(c.RateModel), reason, resetAt.UTC().Format(time.RFC3339))
}

// failureIdentity maps terminal accounting and reconciliation to the last
// attempted candidate, not the account originally chosen by the host.
func failureIdentity(cands []*failoverCandidate, fin failoverResult, routedID, routedUID string) (string, string) {
	id := firstNonEmpty(fin.AuthID, routedID)
	return id, candUID(findCandidate(cands, id), routedUID)
}

func markCandidateCooldown(c *failoverCandidate, resetAt time.Time) {
	for _, key := range candidateCooldownIDs(c) {
		markAuthCooldown(key, resetAt)
	}
}

func resetCandidateCooldown(c *failoverCandidate) {
	for _, key := range candidateCooldownIDs(c) {
		if c.RateModel == "" {
			resetAuthCooldown(key) // legacy/test callers
		} else {
			// A success on model B must not clear model A's quarantine or a
			// concurrently observed account-wide credit rejection.
			resetAuthCooldown(modelCooldownKey(key, c.RateModel))
		}
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
	return !candidateCooldownState(c).NextRetryAt.IsZero()
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
