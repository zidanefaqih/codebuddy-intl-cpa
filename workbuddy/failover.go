// failover.go implements multi-account failover for upstream rate limits
// (HTTP 429 / CodeBuddy code 6004 "usage exceeds frequency limit").
//
// The CPA host's built-in cooldown+retry only applies to in-host executors;
// c-shared plugins execute upstream requests themselves, so a 429 never
// reaches the host selector and no failover happens. This file keeps an
// in-plugin per-account cooldown map and marks accounts that hit 429 so the
// executor retry loop can pick another enabled account for the SAME request.
package main

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// authCooldown is the per-account rate-limit cooldown state.
type authCooldown struct {
	NextRetryAt time.Time // account may be tried again after this instant
	Reason      string    // "429" (or "" when not on cooldown)
}

var (
	cooldownMu sync.Mutex
	cooldowns  = map[string]authCooldown{} // key: auth file name, auth_index, or auth ID
)

// defaultCooldownDuration mirrors the CPA built-in 30-minute cooldown used for
// soft rate limits when the upstream reset time cannot be parsed.
const defaultCooldownDuration = 30 * time.Minute

// markAuthCooldown records that the account reached a rate limit and must not
// be retried until resetAt (zero/past → default 30m cooldown).
func markAuthCooldown(authID string, resetAt time.Time) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	if resetAt.IsZero() || !resetAt.After(time.Now()) {
		resetAt = time.Now().Add(defaultCooldownDuration)
	}
	cooldownMu.Lock()
	cooldowns[authID] = authCooldown{NextRetryAt: resetAt, Reason: "429"}
	cooldownMu.Unlock()
}

// isAuthOnCooldown reports whether the account is still inside its rate-limit
// cooldown window. Expired entries are cleared lazily on read.
func isAuthOnCooldown(authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	c, ok := cooldowns[authID]
	if !ok {
		return false
	}
	if time.Now().Before(c.NextRetryAt) {
		return true
	}
	delete(cooldowns, authID)
	return false
}

// resetAuthCooldown clears any cooldown for the account (used by tests and by
// the executor after a successful call on an account previously marked).
func resetAuthCooldown(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	cooldownMu.Lock()
	delete(cooldowns, authID)
	cooldownMu.Unlock()
}

// rateLimitResetRe matches the CodeBuddy 429 message's reset timestamp, e.g.
// "usage exceeds frequency limit, please try again later, reset at
// 2026-09-06 06:01:58 UTC+8". Only UTC±H offsets are parsed; absent/other
// offsets fall back to the default cooldown.
var rateLimitResetRe = regexp.MustCompile(`reset\s+at\s+(\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2})\s*(UTC(?:\+|-)\d{1,2})?`)

// parseRateLimitResetAt extracts the reset instant from an upstream 429 body.
// Returns the zero time when no parseable reset time is present (callers then
// fall back to defaultCooldownDuration).
func parseRateLimitResetAt(body string) time.Time {
	m := rateLimitResetRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}
	}
	loc := time.FixedZone("UTC+8", 8*60*60) // CodeBuddy reports UTC+8 by default
	if len(m) > 2 && m[2] != "" {
		off := 0
		if h, err := strconv.Atoi(strings.TrimPrefix(strings.TrimPrefix(m[2], "UTC+"), "UTC-")); err == nil {
			if strings.HasPrefix(m[2], "UTC-") {
				off = -h
			} else {
				off = h
			}
		}
		loc = time.FixedZone(m[2], off*60*60)
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], loc)
	if err != nil {
		return time.Time{}
	}
	return t
}

// isRateLimitResponse reports whether an upstream chat response is a soft rate
// limit that should trigger failover: HTTP 429, CodeBuddy code 6004, or
// "frequency limit" / "rate limit" wording in the body. Hard credit errors
// (402/insufficient credit) never trigger failover.
func isRateLimitResponse(statusCode int, body string) bool {
	if isHardCreditError(statusCode, body) {
		return false
	}
	if statusCode == 429 {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "6004") ||
		strings.Contains(lower, "frequency limit") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests")
}
