package main

import (
	"strings"
	"testing"
	"time"
)

func TestIsRiskControlResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"code 11140 as the CN account saw it", 403, `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`, true},
		{"code 11140 without wording", 403, `{"code":11140,"msg":""}`, true},
		{"wording without code", 403, `{"msg":"Your account has been restricted due to unusual activity"}`, true},
		{"credit exhaustion is not risk control", 403, `{"code":14018,"msg":"Credits exhausted"}`, false},
		{"rate limit is not risk control", 429, `{"code":6004,"msg":"frequency limit"}`, false},
		{"channel mismatch is not an account block", 400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, false},
		{"success is never a block", 200, `{"code":11140}`, false},
		{"empty body", 403, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRiskControlResponse(tc.status, tc.body); got != tc.want {
				t.Errorf("isRiskControlResponse(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestClassifyAccountFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   attemptOutcome
	}{
		{"risk control fails over", 403, `{"code":11140,"msg":"request illegal"}`, outcomeRateLimited},
		{"exhaustion is its own outcome", 403, `{"code":14018,"msg":"Credits exhausted"}`, outcomeAccountExhausted},
		{"rate limit fails over", 429, `{"code":6004}`, outcomeRateLimited},
		{"plain upstream error stops", 400, `{"code":1234,"msg":"bad request"}`, outcomeStop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAccountFailure(tc.status, tc.body); got != tc.want {
				t.Errorf("classifyAccountFailure(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestAccountRiskWarningPersistsAndClears(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())

	keys := []string{"auth-index-1", "auth-id-1", "uid-1"}
	markAccountRiskWarning(keys, `{"code":11140,"msg":"request illegal","displayMsg":{"en":"restricted due to unusual activity"}}`)
	// Lookup works through any of the account's identities, and through the
	// state file rather than only the in-process copy.
	for _, key := range keys {
		w, ok := accountWarningFor(key)
		if !ok {
			t.Fatalf("accountWarningFor(%q) = not found, want a warning", key)
		}
		if w.Code != riskControlCode {
			t.Errorf("warning code = %q, want %q", w.Code, riskControlCode)
		}
		if w.Reason != "unusual activity" {
			t.Errorf("warning reason = %q, want unusual activity", w.Reason)
		}
		if w.Since == "" {
			t.Error("warning must record when it was first seen")
		}
	}

	st, err := loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if len(st.Warnings) != len(keys) {
		t.Fatalf("persisted warnings = %d, want %d", len(st.Warnings), len(keys))
	}

	// The account must also be parked on an indefinite block so routing skips
	// it, and the block must carry no probe window a sweep could turn into a
	// recovery attempt.
	if !isAuthOnCooldown("auth-index-1") {
		t.Error("flagged account must be blocked")
	}
	if cd := candidateCooldownState(&failoverCandidate{AuthID: "auth-index-1"}); !cd.ProbeAt.IsZero() && cd.ProbeAt.Before(cd.NextRetryAt) {
		t.Error("risk block must not carry a recovery-probe window")
	}
	if candidateSoftCooldown(&failoverCandidate{AuthID: "auth-index-1"}) {
		t.Error("risk block must not be classified as a soft rate limit")
	}

	clearAccountRiskWarning(keys...)
	if _, ok := accountWarningFor(keys...); ok {
		t.Error("warning survived clear")
	}
	if isAuthOnCooldown("auth-index-1") {
		t.Error("cooldown survived clear")
	}
}

func TestRiskWarningKeepsFirstSighting(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())

	markAccountRiskWarning([]string{"keep-1"}, `{"code":11140,"msg":"restricted due to unusual activity"}`)
	first, _ := accountWarningFor("keep-1")
	// A second sighting must not move the timestamp: it dates the block.
	time.Sleep(10 * time.Millisecond)
	markAccountRiskWarning([]string{"keep-1"}, `{"code":11140,"msg":"restricted due to safety review"}`)
	again, _ := accountWarningFor("keep-1")
	if again.Since != first.Since {
		t.Errorf("Since changed on repeat sighting: %q -> %q", first.Since, again.Since)
	}
	if again.Reason != first.Reason {
		t.Errorf("Reason changed on repeat sighting: %q -> %q", first.Reason, again.Reason)
	}
}

func TestMarkCandidateUnavailableFlagsRiskControl(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
	t.Cleanup(func() { resetAuthCooldown("risk-auth"); resetAuthCooldown("risk-record") })

	cand := &failoverCandidate{AuthID: "risk-auth", RecordID: "risk-record"}
	body := `{"code":11140,"msg":"request illegal"}`
	markCandidateUnavailable(cand, outcomeRateLimited, body)

	w, ok := accountWarningFor("risk-auth")
	if !ok {
		t.Fatal("markCandidateUnavailable did not flag the account")
	}
	if !strings.Contains(w.Reason, "upstream refused") {
		t.Errorf("reason = %q, want the refusal fallback", w.Reason)
	}
	if _, ok := accountWarningFor("risk-record"); !ok {
		t.Error("warning must be recorded under every candidate identity")
	}
}

func TestRiskBlockSurvivesCooldownSweep(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
	t.Cleanup(func() { resetAuthCooldown("sweep-auth") })

	markAccountRiskWarning([]string{"sweep-auth"}, `{"code":11140,"msg":"request illegal","displayMsg":{"en":"restricted due to unusual activity"}}`)

	// The routing sweep prunes any cooldown whose window has elapsed; a block
	// with no expiry must survive that prune instead of reviving the account.
	cd := candidateCooldownState(&failoverCandidate{AuthID: "sweep-auth"})
	if cd.NextRetryAt.IsZero() || !cd.NextRetryAt.After(time.Now().AddDate(50, 0, 0)) {
		t.Fatalf("risk block expiry = %v, want effectively unbounded", cd.NextRetryAt)
	}
	if !isAuthOnCooldown("sweep-auth") {
		t.Error("risk block must stay active after the sweep window passes")
	}

	clearAccountRiskWarning("sweep-auth")
	if isAuthOnCooldown("sweep-auth") {
		t.Error("operator lift must clear the block")
	}
}

func TestRiskRequestRefusalDoesNotTouchAccount(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
	t.Cleanup(func() { resetAuthCooldown("refusal-auth") })

	// 11128 (channel check) and the safety-review verdict refuse the request,
	// not the account: the failover loop must stop without flagging or cooling
	// the credential (a cooldown here would burn a healthy account).
	for _, body := range []string{
		`{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`,
		`{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your content did not pass the safety review"}}`,
	} {
		if outcome := classifyAccountFailure(400, body); outcome != outcomeStop {
			t.Errorf("classifyAccountFailure(%q) = %v, want outcomeStop", body, outcome)
		}
	}
	markCandidateUnavailable(&failoverCandidate{AuthID: "refusal-auth"}, outcomeStop, `{"code":11128,"msg":"Illegal API invocation"}`)
	if _, ok := accountWarningFor("refusal-auth"); ok {
		t.Error("request refusal must not create an account warning")
	}
	if isAuthOnCooldown("refusal-auth") {
		t.Error("request refusal must not cool the account")
	}
}

func TestRiskBanNeedsCodeOrWordingTogether(t *testing.T) {
	// 11140 without any restriction wording still blocks: the code is the
	// account-level verdict even when displayMsg is empty or localized away.
	if !isRiskControlResponse(403, `{"code":11140,"msg":"request illegal"}`) {
		t.Error("bare 11140 must remain an account block")
	}
	// An unrelated upstream saying "restricted" in prose (no 11140, no
	// restriction phrasing) must not put an account in a permanent block.
	if isRiskControlResponse(403, `{"msg":"access to this resource is restricted"}`) {
		t.Error("generic 'restricted' prose must not read as a ban")
	}
}
