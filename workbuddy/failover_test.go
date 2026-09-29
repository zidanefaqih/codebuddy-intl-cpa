package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseRateLimitResetAt_WithUTC8(t *testing.T) {
	body := `usage exceeds frequency limit, please try again later, reset at 2026-09-06 06:01:58 UTC+8`
	got := parseRateLimitResetAt(body)
	want := time.Date(2026, 9, 6, 6, 1, 58, 0, time.FixedZone("UTC+8", 8*60*60))
	if !got.Equal(want) {
		t.Fatalf("parseRateLimitResetAt = %v, want %v", got, want)
	}
}

func TestParseRateLimitResetAt_OtherOffset(t *testing.T) {
	got := parseRateLimitResetAt("reset at 2026-09-06 02:30:00 UTC+2")
	want := time.Date(2026, 9, 6, 2, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	if !got.Equal(want) {
		t.Fatalf("parseRateLimitResetAt = %v, want %v", got, want)
	}
}

func TestParseRateLimitResetAt_NoMatchReturnsZero(t *testing.T) {
	if !parseRateLimitResetAt("some other error").IsZero() {
		t.Fatal("expected zero time for body without reset marker")
	}
	if !parseRateLimitResetAt("").IsZero() {
		t.Fatal("expected zero time for empty body")
	}
}

func TestIsRateLimitResponse(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, "", true},
		{429, `{"code":6004,"msg":"usage exceeds frequency limit"}`, true},
		{200, `{"code":6004,"msg":"usage exceeds frequency limit"}`, true},
		{200, "rate limit exceeded", true},
		{403, "too many requests", true},
		{402, "insufficient credit", false},
		{200, "insufficient credits", false},
		{500, "internal server error", false},
		{200, "ok", false},
	}
	for _, c := range cases {
		if got := isRateLimitResponse(c.status, c.body); got != c.want {
			t.Errorf("isRateLimitResponse(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestCooldownLifecycle(t *testing.T) {
	resetAuthCooldown("auth-a")
	if isAuthOnCooldown("auth-a") {
		t.Fatal("fresh account must not be on cooldown")
	}
	markAuthCooldown("auth-a", time.Now().Add(10*time.Minute))
	if !isAuthOnCooldown("auth-a") {
		t.Fatal("account must be on cooldown after mark")
	}
	resetAuthCooldown("auth-a")
	if isAuthOnCooldown("auth-a") {
		t.Fatal("reset must clear cooldown")
	}
	// Expired cooldown clears lazily on read. (markAuthCooldown refuses past
	// reset times — they fall back to the default 30m window — so simulate the
	// passage of time by writing an expired entry directly.)
	cooldownMu.Lock()
	cooldowns["auth-b"] = authCooldown{NextRetryAt: time.Now().Add(-time.Second), Reason: "429"}
	cooldownMu.Unlock()
	if isAuthOnCooldown("auth-b") {
		t.Fatal("expired cooldown must clear lazily")
	}
	// Empty id is a no-op.
	markAuthCooldown("", time.Now().Add(time.Minute))
	if isAuthOnCooldown("") {
		t.Fatal("empty auth id must never be on cooldown")
	}
}

// TestMarkCooldown_PastResetFallsBackToDefaultWindow locks that a reset time
// already in the past (clock skew) still yields the 30-minute cooldown.
func TestMarkCooldown_PastResetFallsBackToDefaultWindow(t *testing.T) {
	resetAuthCooldown("auth-c")
	defer resetAuthCooldown("auth-c")
	markAuthCooldown("auth-c", time.Now().Add(-5*time.Minute))
	if !isAuthOnCooldown("auth-c") {
		t.Fatal("past reset time must fall back to the default cooldown window")
	}
}

// stubAuthSources replaces the host auth list/get providers for one test.
func stubAuthSources(t *testing.T, files []pluginapi.HostAuthFileEntry, creds map[string]*storedAuth) {
	t.Helper()
	prevList, prevGet := authListSource, authGetSource
	authListSource = func() ([]pluginapi.HostAuthFileEntry, error) { return files, nil }
	authGetSource = func(idx string) (*storedAuth, error) {
		if sa, ok := creds[idx]; ok {
			return sa, nil
		}
		return nil, errors.New("no cred")
	}
	invalidateAuthListCache()
	t.Cleanup(func() {
		authListSource, authGetSource = prevList, prevGet
		invalidateAuthListCache()
	})
}

func testAuthEntry(idx, uid string, disabled bool) pluginapi.HostAuthFileEntry {
	return pluginapi.HostAuthFileEntry{
		AuthIndex: idx,
		ID:        uid,
		Name:      "workbuddy-" + uid + ".json",
		Disabled:  disabled,
		Status:    "ok",
	}
}

func TestBuildFailoverPool_RoutedAndOthers(t *testing.T) {
	files := []pluginapi.HostAuthFileEntry{
		testAuthEntry("idx-1", "uid-1", false),
		testAuthEntry("idx-2", "uid-2", false),
		testAuthEntry("idx-3", "uid-3", true), // disabled — skipped
	}
	creds := map[string]*storedAuth{
		"idx-1": {Auth: storedTokens{Domain: "https://www.workbuddy.ai"}, Account: storedAccount{UID: "uid-1"}},
		"idx-2": {Auth: storedTokens{Domain: "https://www.workbuddy.ai"}, Account: storedAccount{UID: "uid-2"}},
	}
	stubAuthSources(t, files, creds)

	// Routing to idx-1: primary = idx-1, one fallback = idx-2 (idx-3 disabled).
	pool := buildFailoverPool("uid-1")
	if pool.Primary == nil || pool.Primary.AuthID != "idx-1" {
		t.Fatalf("primary = %+v, want idx-1", pool.Primary)
	}
	if len(pool.Others) != 1 || pool.Others[0].AuthID != "idx-2" {
		t.Fatalf("others = %+v, want [idx-2]", pool.Others)
	}
}

func TestBuildFailoverPool_SkipsCooldown(t *testing.T) {
	files := []pluginapi.HostAuthFileEntry{
		testAuthEntry("idx-1", "uid-1", false),
		testAuthEntry("idx-2", "uid-2", false),
	}
	creds := map[string]*storedAuth{
		"idx-1": {Auth: storedTokens{Domain: "https://www.workbuddy.ai"}, Account: storedAccount{UID: "uid-1"}},
		"idx-2": {Auth: storedTokens{Domain: "https://www.workbuddy.ai"}, Account: storedAccount{UID: "uid-2"}},
	}
	stubAuthSources(t, files, creds)

	markAuthCooldown("idx-2", time.Now().Add(5*time.Minute))
	defer resetAuthCooldown("idx-2")

	pool := buildFailoverPool("uid-1")
	if len(pool.Others) != 0 {
		t.Fatalf("cooldown fallback must be excluded, others = %+v", pool.Others)
	}
}

// TestRunFailover_RetriesOn429 verifies the retry order and cooldown marking
// when the routed account answers 429 and the fallback succeeds.
func TestRunFailover_RetriesOn429(t *testing.T) {
	resetAuthCooldown("idx-1")
	resetAuthCooldown("idx-2")
	defer func() { resetAuthCooldown("idx-1"); resetAuthCooldown("idx-2") }()

	cands := []*failoverCandidate{
		{AuthID: "idx-1", Auth: &storedAuth{}},
		{AuthID: "idx-2", Auth: &storedAuth{}},
	}
	var order []string
	fin := runFailover(cands, func(cand *failoverCandidate) (attemptOutcome, int, string, error) {
		order = append(order, cand.AuthID)
		if cand.AuthID == "idx-1" {
			return outcomeRateLimited, 429, `usage exceeds frequency limit, reset at 2099-01-01 00:00:00 UTC+8`, errors.New("upstream 429")
		}
		return outcomeSuccess, 0, "", nil
	})
	if fin.Outcome != outcomeSuccess || fin.AuthID != "idx-2" {
		t.Fatalf("fin = %+v, want success on idx-2", fin)
	}
	if len(order) != 2 || order[0] != "idx-1" || order[1] != "idx-2" {
		t.Fatalf("attempt order = %v, want [idx-1 idx-2]", order)
	}
	if !isAuthOnCooldown("idx-1") {
		t.Fatal("rate-limited primary must be marked on cooldown")
	}
}

// TestRunFailover_All429 verifies the loop exhausts candidates, marks each,
// and reports the last failure.
func TestRunFailover_All429(t *testing.T) {
	resetAuthCooldown("idx-1")
	resetAuthCooldown("idx-2")
	defer func() { resetAuthCooldown("idx-1"); resetAuthCooldown("idx-2") }()

	cands := []*failoverCandidate{
		{AuthID: "idx-1", Auth: &storedAuth{}},
		{AuthID: "idx-2", Auth: &storedAuth{}},
	}
	var order []string
	fin := runFailover(cands, func(cand *failoverCandidate) (attemptOutcome, int, string, error) {
		order = append(order, cand.AuthID)
		return outcomeRateLimited, 429, "rate limited", errors.New("upstream 429")
	})
	if fin.Outcome != outcomeRateLimited {
		t.Fatalf("fin.Outcome = %v, want outcomeRateLimited", fin.Outcome)
	}
	if len(order) != 2 {
		t.Fatalf("attempt order = %v, want both tried", order)
	}
	if !isAuthOnCooldown("idx-1") || !isAuthOnCooldown("idx-2") {
		t.Fatal("both accounts must be on cooldown")
	}
}

// TestRunFailover_StopsOnBusinessError verifies non-rate-limit failures are
// never retried on another account.
func TestRunFailover_StopsOnBusinessError(t *testing.T) {
	cands := []*failoverCandidate{
		{AuthID: "idx-1", Auth: &storedAuth{}},
		{AuthID: "idx-2", Auth: &storedAuth{}},
	}
	var order []string
	fin := runFailover(cands, func(cand *failoverCandidate) (attemptOutcome, int, string, error) {
		order = append(order, cand.AuthID)
		return outcomeStop, 402, "insufficient credit", errors.New("upstream 402")
	})
	if fin.Outcome != outcomeStop || len(order) != 1 || order[0] != "idx-1" {
		t.Fatalf("fin = %+v order = %v, want immediate stop on idx-1", fin, order)
	}
}

// TestRunFailover_EmptyStreamNoRetry locks the revision requirement: an
// empty_stream (upstream closed before first payload) is NOT an account
// problem — retrying it would double-bill credits. The loop must stop after
// the first attempt and must NOT mark the account on cooldown.
func TestRunFailover_EmptyStreamNoRetry(t *testing.T) {
	resetAuthCooldown("idx-1")
	resetAuthCooldown("idx-2")
	defer func() { resetAuthCooldown("idx-1"); resetAuthCooldown("idx-2") }()

	cands := []*failoverCandidate{
		{AuthID: "idx-1", Auth: &storedAuth{}},
		{AuthID: "idx-2", Auth: &storedAuth{}},
	}
	var order []string
	fin := runFailover(cands, func(cand *failoverCandidate) (attemptOutcome, int, string, error) {
		order = append(order, cand.AuthID)
		return outcomeStop, 0, "", errEmptyStream
	})
	if fin.Outcome != outcomeStop {
		t.Fatalf("fin.Outcome = %v, want outcomeStop", fin.Outcome)
	}
	if len(order) != 1 || order[0] != "idx-1" {
		t.Fatalf("attempt order = %v, want single attempt on idx-1 (no retry on empty_stream)", order)
	}
	if !isEmptyStreamError(fin.Err) {
		t.Fatalf("fin.Err = %v, want errEmptyStream surfaced", fin.Err)
	}
	if isAuthOnCooldown("idx-1") || isAuthOnCooldown("idx-2") {
		t.Fatal("empty_stream must NOT mark accounts on cooldown")
	}
}

// TestClassifyNonStream_EmptyStreamIsStop verifies the classifier used by the
// non-stream executor path treats errEmptyStream as a stop (never rate-limit).
func TestClassifyNonStream_EmptyStreamIsStop(t *testing.T) {
	outcome, status, body, err := classifyNonStream(nil, errEmptyStream)
	if outcome != outcomeStop {
		t.Fatalf("outcome = %v, want outcomeStop", outcome)
	}
	if status != 0 || body != "" {
		t.Fatalf("status/body = %d/%q, want 0/empty", status, body)
	}
	if !isEmptyStreamError(err) {
		t.Fatalf("err = %v, want errEmptyStream", err)
	}
}

// TestRunStreamFailover_NoRetryAfterChunkEmitted locks the invariant that a
// stream which already emitted chunks is never replayed on another account.
func TestRunStreamFailover_NoRetryAfterChunkEmitted(t *testing.T) {
	cands := []*failoverCandidate{
		{AuthID: "idx-1", Auth: &storedAuth{}},
		{AuthID: "idx-2", Auth: &storedAuth{}},
	}
	var order []string
	fin := runStreamFailover(cands, func(cand *failoverCandidate) (bool, int, error) {
		order = append(order, cand.AuthID)
		return true, 200, errors.New("mid-stream read error after chunks")
	})
	if fin.Outcome != outcomeStop || !fin.Emitted {
		t.Fatalf("fin = %+v, want stop with emitted=true", fin)
	}
	if len(order) != 1 {
		t.Fatalf("attempt order = %v, want single attempt", order)
	}
}

// TestRunStreamFailover_RetriesPreChunk429 verifies a 429 observed before the
// first chunk triggers a fallback attempt.
func TestRunStreamFailover_RetriesPreChunk429(t *testing.T) {
	resetAuthCooldown("idx-1")
	resetAuthCooldown("idx-2")
	defer func() { resetAuthCooldown("idx-1"); resetAuthCooldown("idx-2") }()

	cands := []*failoverCandidate{
		{AuthID: "idx-1", Auth: &storedAuth{}},
		{AuthID: "idx-2", Auth: &storedAuth{}},
	}
	var order []string
	fin := runStreamFailover(cands, func(cand *failoverCandidate) (bool, int, error) {
		order = append(order, cand.AuthID)
		if cand.AuthID == "idx-1" {
			return false, 429, errors.New(`upstream 429: usage exceeds frequency limit`)
		}
		return false, 0, nil
	})
	if fin.Outcome != outcomeSuccess || fin.AuthID != "idx-2" {
		t.Fatalf("fin = %+v, want success on idx-2", fin)
	}
	if len(order) != 2 || order[0] != "idx-1" || order[1] != "idx-2" {
		t.Fatalf("attempt order = %v, want [idx-1 idx-2]", order)
	}
}

// TestRunStreamFailover_EmptyStreamNoRetry locks the revision requirement for
// the async path: empty_stream before any chunk is NOT retried on another
// account (double-billing) and does NOT mark cooldown.
func TestRunStreamFailover_EmptyStreamNoRetry(t *testing.T) {
	resetAuthCooldown("idx-1")
	resetAuthCooldown("idx-2")
	defer func() { resetAuthCooldown("idx-1"); resetAuthCooldown("idx-2") }()

	cands := []*failoverCandidate{
		{AuthID: "idx-1", Auth: &storedAuth{}},
		{AuthID: "idx-2", Auth: &storedAuth{}},
	}
	var order []string
	fin := runStreamFailover(cands, func(cand *failoverCandidate) (bool, int, error) {
		order = append(order, cand.AuthID)
		return false, 0, errEmptyStream
	})
	if fin.Outcome != outcomeStop {
		t.Fatalf("fin.Outcome = %v, want outcomeStop", fin.Outcome)
	}
	if len(order) != 1 || order[0] != "idx-1" {
		t.Fatalf("attempt order = %v, want single attempt on idx-1 (no retry on empty_stream)", order)
	}
	if !isEmptyStreamError(fin.Err) {
		t.Fatalf("fin.Err = %v, want errEmptyStream surfaced", fin.Err)
	}
	if fin.Emitted {
		t.Fatal("empty_stream attempt must not be marked emitted")
	}
	if isAuthOnCooldown("idx-1") || isAuthOnCooldown("idx-2") {
		t.Fatal("empty_stream must NOT mark accounts on cooldown")
	}
}

// TestIsEmptyStreamError covers the sentinel check.
func TestIsEmptyStreamError(t *testing.T) {
	if !isEmptyStreamError(errEmptyStream) {
		t.Fatal("errEmptyStream must be recognized")
	}
	if isEmptyStreamError(errors.New("other")) || isEmptyStreamError(nil) {
		t.Fatal("non-empty-stream errors must not be recognized")
	}
}

func TestUpstreamStatusError_Formatting(t *testing.T) {
	e := &upstreamStatusError{status: 429, body: fmt.Sprintf("usage exceeds frequency limit reset at %s", time.Now().Format("2006-01-02 15:04:05")) + " UTC+8"}
	msg := e.Error()
	if len(msg) == 0 || !strings.Contains(msg, "upstream 429") {
		t.Fatalf("unexpected error format: %q", msg)
	}
}
