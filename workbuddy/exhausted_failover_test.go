package main

import (
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func exhaustedCandidates(t *testing.T) []*failoverCandidate {
	t.Helper()
	cands := []*failoverCandidate{
		{AuthID: "exhaust-a", RecordID: "record-a", Aliases: []string{"workbuddy-uid-a.json", "workbuddy-record-a.json"}, Auth: &storedAuth{Auth: storedTokens{Domain: "https://www.codebuddy.ai"}, Account: storedAccount{UID: "uid-a"}}},
		{AuthID: "exhaust-b", RecordID: "record-b", Aliases: []string{"workbuddy-uid-b.json", "workbuddy-record-b.json"}, Auth: &storedAuth{Auth: storedTokens{Domain: "https://www.codebuddy.ai"}, Account: storedAccount{UID: "uid-b"}}},
	}
	for _, c := range cands {
		resetCandidateCooldown(c)
	}
	t.Cleanup(func() {
		for _, c := range cands {
			resetCandidateCooldown(c)
		}
	})
	return cands
}

func TestAccountExhaustionClassifier(t *testing.T) {
	for _, tt := range []struct {
		status int
		body   string
		want   bool
	}{
		{429, `{"error":{"data":{"code":14018,"msg":"Credits exhausted"}}}`, true},
		{403, `{"code": 14018,"msg":"localized"}`, true},
		{402, "payment required", true},
		{403, "insufficient credits", true},
		{429, `{"code":6004,"msg":"frequency limit"}`, false},
		{200, "Credits exhausted", false},
		{0, "Credits exhausted", false},
		{500, `{"code":140180}`, false},
		{400, "invalid tool schema", false},
	} {
		if got := isAccountExhaustedResponse(tt.status, tt.body); got != tt.want {
			t.Errorf("classify(%d,%q)=%v", tt.status, tt.body, got)
		}
	}
}

func TestExhaustedFailoverRetriesAndSkipsNextRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			cands := exhaustedCandidates(t)
			body := `{"error":{"data":{"code":14018,"msg":"Credits exhausted"}}}`
			var tried []string
			attempt := func(c *failoverCandidate) (bool, int, error) {
				tried = append(tried, c.AuthID)
				if c == cands[0] {
					return false, 429, &upstreamStatusError{status: 429, body: body}
				}
				return false, 200, nil
			}
			run := func() failoverResult {
				if stream {
					return runStreamFailover(cands, attempt)
				}
				return runFailover(cands, func(c *failoverCandidate) (attemptOutcome, int, string, error) {
					_, _, err := attempt(c)
					return classifyNonStream(nil, err)
				})
			}
			fin := run()
			if fin.Outcome != outcomeSuccess || fin.AuthID != "exhaust-b" || len(tried) != 2 {
				t.Fatalf("first result=%+v tried=%v", fin, tried)
			}
			for _, key := range candidateCooldownIDs(cands[0]) {
				cooldownMu.Lock()
				cd := cooldowns[key]
				cooldownMu.Unlock()
				if cd.Reason != "credits_exhausted" || time.Until(cd.NextRetryAt) < 29*time.Minute || time.Until(cd.NextRetryAt) > 31*time.Minute {
					t.Fatalf("bad quarantine %s: %+v", key, cd)
				}
			}
			tried = nil
			fin = run()
			if fin.Outcome != outcomeSuccess || len(tried) != 1 || tried[0] != "exhaust-b" {
				t.Fatalf("next result=%+v tried=%v", fin, tried)
			}
		})
	}
}

func TestAllExhaustedKeepsLastCandidateIdentity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			cands := exhaustedCandidates(t)
			attempt := func(c *failoverCandidate) (bool, int, error) {
				return false, 402, &upstreamStatusError{status: 402, body: "insufficient credits"}
			}
			var fin failoverResult
			if stream {
				fin = runStreamFailover(cands, attempt)
			} else {
				fin = runFailover(cands, func(c *failoverCandidate) (attemptOutcome, int, string, error) {
					_, _, err := attempt(c)
					return classifyNonStream(nil, err)
				})
			}
			id, uid := failureIdentity(cands, fin, "exhaust-a", "uid-a")
			if fin.Outcome != outcomeAccountExhausted || fin.Status != 402 || len(fin.Tried) != 2 || id != "exhaust-b" || uid != "uid-b" {
				t.Fatalf("result=%+v identity=%s/%s", fin, id, uid)
			}
			for _, c := range cands {
				if !candidateOnCooldown(c) {
					t.Fatalf("not quarantined: %s", c.AuthID)
				}
			}
		})
	}
}

func TestExhaustedPartialStreamNeverReplays(t *testing.T) {
	cands := exhaustedCandidates(t)
	fin := runStreamFailover(cands, func(c *failoverCandidate) (bool, int, error) { return true, 429, errors.New("Credits exhausted") })
	if fin.Outcome != outcomeStop || !fin.Emitted || len(fin.Tried) != 1 || candidateOnCooldown(cands[0]) {
		t.Fatalf("partial result=%+v", fin)
	}
}

func TestNoChunkFailureUsesFinalUID(t *testing.T) {
	cands := exhaustedCandidates(t)
	fin := runStreamFailover(cands, func(c *failoverCandidate) (bool, int, error) {
		if c == cands[0] {
			return false, 429, errors.New("frequency limit")
		}
		return false, 403, errors.New("account forbidden")
	})
	id, uid := failureIdentity(cands, fin, "exhaust-a", "uid-a")
	if fin.Outcome != outcomeStop || id != "exhaust-b" || uid != "uid-b" {
		t.Fatalf("result=%+v identity=%s/%s", fin, id, uid)
	}
}

func TestRealmPoolHonorsQuarantineAliases(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
	cands := exhaustedCandidates(t)
	files := []pluginapi.HostAuthFileEntry{testAuthEntry("exhaust-a", "record-a", false), testAuthEntry("exhaust-b", "record-b", false), testAuthEntry("cn-only", "cn-uid", false)}
	creds := map[string]*storedAuth{"exhaust-a": cands[0].Auth, "exhaust-b": cands[1].Auth, "cn-only": {Auth: storedTokens{Domain: "https://copilot.tencent.com"}, Account: storedAccount{UID: "cn-uid"}}}
	stubAuthSources(t, files, creds)
	// UID-only quarantine must not be bypassed by a prefixed route.
	markAuthCooldownReason("uid-a", time.Time{}, "credits_exhausted")
	pool, _, err := buildRealmFailoverPool("cb/gpt-6.1-sol", "exhaust-a", storageJSONForAuth(cands[0].Auth), realmGlobal)
	if err != nil {
		t.Fatal(err)
	}
	got := pool.ordered(nil)
	if len(got) != 1 || got[0].AuthID != "exhaust-b" || got[0].RecordID != "record-b" {
		t.Fatalf("pool=%+v", got)
	}
	if files[0].Disabled {
		t.Fatal("quarantine must not disable auth")
	}
}

func TestEmptyPoolReturnsUsableTerminalStatus(t *testing.T) {
	for _, fin := range []failoverResult{runFailover(nil, nil), runStreamFailover(nil, nil)} {
		if fin.Status != 429 || fin.Body == "" || fin.Err == nil {
			t.Fatalf("empty result=%+v", fin)
		}
	}
}

// A burst of parallel requests can rate-limit every account of a realm at once.
// The pool must stay routable (retry the routed account) instead of answering
// 500 for the whole cooldown window.
func TestRealmPoolStaysRoutableWhenEveryAccountCools(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
	cands := exhaustedCandidates(t)
	files := []pluginapi.HostAuthFileEntry{testAuthEntry("exhaust-a", "record-a", false), testAuthEntry("exhaust-b", "record-b", false), testAuthEntry("cn-only", "cn-uid", false)}
	creds := map[string]*storedAuth{"exhaust-a": cands[0].Auth, "exhaust-b": cands[1].Auth, "cn-only": {Auth: storedTokens{Domain: "https://copilot.tencent.com"}, Account: storedAccount{UID: "cn-uid"}}}
	stubAuthSources(t, files, creds)
	markAuthCooldownReason("exhaust-a", time.Time{}, "429")
	markAuthCooldownReason("exhaust-b", time.Time{}, "429")
	pool, storage, err := buildRealmFailoverPool("cb/gpt-6.1-sol", "exhaust-a", storageJSONForAuth(cands[0].Auth), realmGlobal)
	if err != nil {
		t.Fatalf("a cooling realm must stay routable: %v", err)
	}
	got := pool.ordered(storage)
	if len(got) != 1 || got[0].AuthID != "exhaust-a" || got[0].Auth == nil {
		t.Fatalf("pool=%+v", got)
	}
	// Without a routed match the pool picks whichever account recovers first.
	pool, _, err = buildRealmFailoverPool("cb/gpt-6.1-sol", "missing", storageJSONForAuth(cands[0].Auth), realmGlobal)
	if err != nil || pool.Primary == nil {
		t.Fatalf("pool=%+v err=%v", pool, err)
	}
	// A realm with no account at all still fails fast.
	creds["exhaust-a"] = &storedAuth{Auth: storedTokens{Domain: "https://copilot.tencent.com"}, Account: storedAccount{UID: "cn-uid"}}
	creds["exhaust-b"] = &storedAuth{Auth: storedTokens{Domain: "https://copilot.tencent.com"}, Account: storedAccount{UID: "cn-uid-2"}}
	stubAuthSources(t, files, creds)
	if _, _, err := buildRealmFailoverPool("cb/gpt-6.1-sol", "exhaust-a", nil, realmGlobal); err == nil {
		t.Fatal("a realm without any Global account must still error")
	}
}

// Soft rate limits must recover quickly; credit exhaustion keeps the long backoff.
func TestRateLimitCooldownIsCappedButCreditBackoffIsNot(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
	cands := exhaustedCandidates(t)
	markCandidateUnavailable(cands[0], outcomeRateLimited, "")
	if until := time.Until(authCooldownUntil(cands[0].AuthID)); until <= 0 || until > maxRateLimitCooldown+time.Minute {
		t.Fatalf("429 cooldown not capped: %s", until)
	}
	for _, c := range cands {
		resetAuthCooldown(c.AuthID)
	}
	markCandidateUnavailable(cands[0], outcomeAccountExhausted, "")
	if until := time.Until(authCooldownUntil(cands[0].AuthID)); until < 25*time.Minute {
		t.Fatalf("credit exhaustion lost its long backoff: %s", until)
	}
}
