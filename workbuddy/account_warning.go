// Account risk-control warnings (upstream chat restriction, code 11140).
//
// The upstream can block an account from *generating* — "request illegal" /
// "account has been restricted due to unusual activity" — while billing,
// check-in and the OAuth endpoints keep answering normally for the same token.
// The credential is valid; the account simply cannot chat, and no retry, token
// refresh, re-import or re-enable fixes that. The state therefore has to be
// visible: without it the panel shows "Active" while every request routed to
// that account fails.
//
// A warning is recorded when the code is first seen, persisted in
// workbuddy-state.json so it survives restarts (the failover cooldown is
// process-local), and parks the account on a long cooldown so routing stops
// selecting it. The operator clears it once the account was checked upstream or
// replaced.
package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	// riskControlCode is the upstream code for "this account may not generate".
	// Distinct from 11128 (channel/prompt mismatch) and 14018 (credits).
	riskControlCode = "11140"
)

// riskControlCodeRe matches the code inside an upstream envelope. It is matched
// on the body because the failure arrives as HTTP 403 and the code is the only
// stable field — the human message is localized.
var riskControlCodeRe = regexp.MustCompile(`"code"\s*:\s*11140\b`)

// riskBanWordingRe matches the body of an upstream that restricted the
// *account*, not the request. The buddy family reuses "request illegal" for two
// unrelated rejections and the difference is only in the body: 11140 with
// account-restriction wording (plus its localized variants) is a ban — the
// account is refused, not throttled — while a safety-review verdict refuses the
// *request* and leaves the credential healthy. An unrelated upstream that
// happens to say "restricted" in prose cannot put an account in a permanent
// block, because the code must match too.
var riskBanWordingRe = regexp.MustCompile(
	`restricted due to unusual activity|been restricted|has been banned|unusual activity|appeal this restriction|账号存在异常行为|帳號存在異常行為`)

// riskReviewWordingRe matches a request-side refusal: the verdict names the
// content, not the account, so the credential stays eligible for other traffic.
var riskReviewWordingRe = regexp.MustCompile(`safety review|安全审核`)

// accountWarning is the panel-facing record of a flagged account.
type accountWarning struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
	Since  string `json:"since"`
}

// isRiskControlResponse reports whether an upstream failure is the account-level
// chat restriction rather than a credit or rate-limit problem. A status code of
// 0 means "unknown, judge the body alone" (the failover helpers only carry the
// body in places).
func isRiskControlResponse(statusCode int, body string) bool {
	if statusCode > 0 && statusCode < 400 {
		return false
	}
	if strings.TrimSpace(body) == "" {
		return false
	}
	if riskControlCodeRe.MatchString(body) {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "been restricted") ||
		strings.Contains(lower, "restricted due to unusual activity")
}

// riskControlReason turns the upstream message into a short panel reason.
func riskControlReason(body string) string {
	lower := strings.ToLower(body)
	switch {
	case riskReviewWordingRe.MatchString(lower):
		return "safety review"
	case riskBanWordingRe.MatchString(lower):
		return "unusual activity"
	}
	return "upstream refused the account"
}

// isRiskRequestRefusal reports whether the upstream refused this *request* —
// a verdict on the content, not the account. Cooling the account here would
// burn a healthy credential on the gateway's own fault; the correct response
// is to stop retrying the request verbatim and leave the account alone.
func isRiskRequestRefusal(statusCode int, body string) bool {
	if statusCode > 0 && statusCode < 400 {
		return false
	}
	lower := strings.ToLower(body)
	return riskReviewWordingRe.MatchString(lower) ||
		(strings.Contains(lower, "11128") && strings.Contains(lower, "illegal"))
}

// markAccountRiskWarning records the warning under every key the account is
// known by and takes it out of routing until the operator clears it. The state
// write is best effort; the in-process block still protects traffic if the
// disk write fails.
//
// The block is deliberately never timed: a 11140 restriction does not expire —
// the account is refused, not throttled, so a window only re-sends traffic to
// it (and burns sibling accounts through the retry budget) every time the
// window lapses. Recovery probes are reserved for soft rate limits; a warning
// is never one.
func markAccountRiskWarning(keys []string, body string) {
	keys = uniqueNonEmptyKeys(keys)
	if len(keys) == 0 {
		return
	}
	w := accountWarning{
		Code:   riskControlCode,
		Reason: riskControlReason(body),
		Since:  time.Now().UTC().Format(time.RFC3339),
	}
	_ = mutateState(func(st *wbStateFile) error {
		if st.Warnings == nil {
			st.Warnings = map[string]accountWarning{}
		}
		for _, k := range keys {
			if old, ok := st.Warnings[k]; ok && old.Since != "" {
				continue // keep the first sighting: it dates the block
			}
			st.Warnings[k] = w
		}
		return nil
	})
	for _, k := range keys {
		blockAuthIndefinitely(k, "risk_control")
	}
	fmt.Fprintf(os.Stderr, "[workbuddy] risk-control: auth=%q flagged reason=%q (no re-probe until restart or state clear)\n",
		redactSecrets(keys[0]), w.Reason)
}

// markCandidateRiskBlocked is the failover-loop entry point: it flags the
// candidate under every identity it is reachable by.
func markCandidateRiskBlocked(c *failoverCandidate, body string) {
	if c == nil {
		return
	}
	markAccountRiskWarning(candidateCooldownIDs(c), body)
}

// accountWarningFor returns the warning for any of the account's keys.
func accountWarningFor(keys ...string) (accountWarning, bool) {
	st, err := loadState()
	if err != nil || st.Warnings == nil {
		return accountWarning{}, false
	}
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if w, ok := st.Warnings[k]; ok {
			return w, true
		}
	}
	return accountWarning{}, false
}

// clearAccountRiskWarning removes the flag and its cooldown — used after the
// operator checked the account upstream or swapped it out.
func clearAccountRiskWarning(keys ...string) {
	keys = uniqueNonEmptyKeys(keys)
	if len(keys) == 0 {
		return
	}
	_ = mutateState(func(st *wbStateFile) error {
		for _, k := range keys {
			delete(st.Warnings, k)
		}
		return nil
	})
	for _, k := range keys {
		resetAuthCooldown(k)
	}
}

// uniqueNonEmptyKeys trims, drops empties and de-duplicates while keeping order.
func uniqueNonEmptyKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// classifyAccountFailure maps an upstream failure to the failover outcome used
// by every attempt loop, so the three loops cannot drift apart. A request
// refusal (11128 / safety review) stops the retry loop without touching the
// account; the account-level ban gets its own long block and warning via
// markCandidateUnavailable.
func classifyAccountFailure(status int, body string) attemptOutcome {
	switch {
	case isAccountExhaustedResponse(status, body):
		return outcomeAccountExhausted
	case isRiskRequestRefusal(status, body):
		return outcomeStop
	case isRiskControlResponse(status, body):
		return outcomeRateLimited
	case upstreamErrorCode(body) == modelNotAllowedCode:
		return outcomeModelNotAllowed
	case isRateLimitResponse(status, body):
		return outcomeRateLimited
	}
	return outcomeStop
}
