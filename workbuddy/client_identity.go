// Client identity stamped on every upstream CodeBuddy request.
//
// The upstream validates the caller's client identity, not just the bearer
// token: the Intl realm (codebuddy.ai) serves the IDE channel and the CN realm
// (copilot.tencent.com) the CLI channel, and a request that arrives without the
// matching identity headers is refused with `11128 "Illegal API invocation from
// an unapproved channel"` — the same code that a blocked system prompt draws.
//
// Two gateways that speak to this upstream (Cartethyia and VansRouter) both
// send the full identity family — `X-IDE-Type`/`X-IDE-Name`,
// `x-codebuddy-request`, `X-Root-Request-ID`/`X-Request-ID` and the B3 trace
// pair — and both stamp a client version that is tracked from the publisher's
// release feed rather than pinned, because a stale version is itself a signal
// the upstream can reject. Earlier builds of this plugin sent only
// `CLI/<pinned version>` plus `X-Product`, which is the gap this file closes.
//
// Version resolution is deliberately non-blocking: a request always gets the
// cached (or fallback) version immediately, and the feed is refreshed in the
// background at most once per TTL. A registry outage must never stall traffic.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// codeBuddyVersionURL is the npm release feed for the CodeBuddy CLI. Both
	// identities are stamped with a version from this feed; the identity prefix
	// (IDE vs CLI) is what differs per realm.
	codeBuddyVersionURL = "https://registry.npmjs.org/@tencent-ai/codebuddy-code/latest"

	// Pinned fallbacks, used until the first successful feed fetch. The Intl
	// fallback matches the version the reference gateways ship; the CN fallback
	// keeps the long-standing pinned value so existing CN traffic is unchanged
	// if the feed is unreachable.
	fallbackIntlClientVersion = "2.168.2"
	fallbackCNClientVersion   = "2.63.2"

	clientVersionTTL     = 6 * time.Hour
	clientVersionTimeout = 8 * time.Second
)

// clientVersionHTTPClient is deliberately separate from sharedHTTPClient():
// the upstream client is stubbed in tests and carries the chat transport, and a
// background version poll must never ride the same connection pool (or a test
// mock) as live traffic.
var clientVersionHTTPClient = &http.Client{Timeout: clientVersionTimeout}

var (
	clientVersionMu        sync.Mutex
	clientVersionValue     string
	clientVersionFetchedAt time.Time
	clientVersionFetching  int32 // single-flight guard for the background refresh

	// clientVersionNow is a seam for tests.
	clientVersionNow = time.Now

	// clientVersionRefresh is a seam for tests: the resolver schedules the
	// background fetch through it so a unit test never opens a socket.
	clientVersionRefresh = refreshClientVersion
)

// identityFor returns the client identity the account's realm expects: the IDE
// channel for Intl accounts, the CLI channel for CN (and legacy) accounts.
func identityFor(sa *storedAuth) string {
	if sa != nil && isGlobalDomain(sa.Auth.Domain) {
		return "IDE"
	}
	return "CLI"
}

// clientVersionEnv returns an operator override, checked per realm first so a
// single test can pin one realm without touching the other.
func clientVersionEnv(global bool) string {
	if global {
		if v := strings.TrimSpace(os.Getenv("WB_CLIENT_VERSION_INTL")); v != "" {
			return v
		}
	} else if v := strings.TrimSpace(os.Getenv("WB_CLIENT_VERSION_CN")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("WB_CLIENT_VERSION"))
}

// fallbackClientVersion is the pinned version for a realm.
func fallbackClientVersion(global bool) string {
	if global {
		return fallbackIntlClientVersion
	}
	return fallbackCNClientVersion
}

// clientVersionFor returns the version to stamp for a realm. It never blocks on
// the network: a stale cache schedules a background refresh and the caller gets
// the cached value (or the pinned fallback) right away.
func clientVersionFor(global bool) string {
	if v := clientVersionEnv(global); v != "" {
		return v
	}
	clientVersionMu.Lock()
	v := clientVersionValue
	fetchedAt := clientVersionFetchedAt
	clientVersionMu.Unlock()
	if v == "" || clientVersionNow().Sub(fetchedAt) > clientVersionTTL {
		go clientVersionRefresh()
	}
	if v == "" {
		return fallbackClientVersion(global)
	}
	return v
}

// refreshClientVersion fetches the published CodeBuddy CLI version. Failures
// are silent and leave the previous value in place; the next caller retries
// after the TTL, and the single-flight guard keeps a burst from fanning out.
func refreshClientVersion() {
	if !atomic.CompareAndSwapInt32(&clientVersionFetching, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&clientVersionFetching, 0)

	ctx, cancel := context.WithTimeout(context.Background(), clientVersionTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codeBuddyVersionURL, nil)
	if err != nil {
		return
	}
	// A plain browser-ish UA is fine here: the registry is public and not gated.
	req.Header.Set("User-Agent", "Mozilla/5.0 (CodeBuddy client version check)")
	req.Header.Set("Accept", "application/json")

	resp, err := clientVersionHTTPClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return
	}
	var payload struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return
	}
	version := strings.TrimSpace(payload.Version)
	if !semverish(version) {
		return
	}
	clientVersionMu.Lock()
	clientVersionValue = version
	clientVersionFetchedAt = clientVersionNow()
	clientVersionMu.Unlock()
}

// semverish accepts the `x.y.z` shape the upstream versions use, so a feed
// hiccup that returns HTML or an empty field cannot replace a good version.
func semverish(v string) bool {
	if v == "" {
		return false
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false
	}
	if len(parts) > 3 {
		parts = parts[:3]
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// buildClientUA renders the identity-qualified user agent, e.g.
// `IDE/2.168.2 CodeBuddy/2.168.2`.
func buildClientUA(identity, version string) string {
	return identity + "/" + version + " CodeBuddy/" + version
}

// deviceTokenFor returns the device token to forward, if the account carries
// one. Imported CodeBuddy credentials usually do not, and the upstream accepts
// a request without it; the header is simply omitted rather than invented.
// WB_DEVICE_TOKEN overrides every account (the same escape hatch the reference
// gateway offers through its shared device-token file).
func deviceTokenFor(sa *storedAuth) string {
	if v := strings.TrimSpace(os.Getenv("WB_DEVICE_TOKEN")); v != "" {
		return v
	}
	if sa == nil || len(sa.rawJSON) == 0 {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(sa.rawJSON, &doc) != nil {
		return ""
	}
	for _, path := range [][]string{
		{"auth", "deviceToken"},
		{"auth", "device_token"},
		{"deviceToken"},
		{"device_token"},
	} {
		var cur any = doc
		for _, key := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				cur = nil
				break
			}
			cur = m[key]
		}
		if s, ok := cur.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// `x-stainless-*` is the OpenAI Node SDK 6.25.0 fingerprint: every official
// client built on that SDK reports it on chat calls, so its absence is a
// fingerprint gap no real client produces. The values are pinned instead of
// host-derived: a plugin running on Windows/ARM would otherwise advertise an
// OS/arch pair the upstream's risk model has never seen for this client brand.
const (
	stainlessPackageVersion = "6.25.0"
	stainlessRuntimeVersion = "v24.21.0"
)

// applyIdentityHeaders stamps the per-request client identity the upstream
// validates. It is the single place that decides what an official client looks
// like, so a future upstream change has one edit site.
func applyIdentityHeaders(req *http.Request, sa *storedAuth) {
	global := sa != nil && isGlobalDomain(sa.Auth.Domain)
	identity := identityFor(sa)
	req.Header.Set("User-Agent", buildClientUA(identity, clientVersionFor(global)))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-IDE-Type", identity)
	req.Header.Set("X-IDE-Name", identity)
	// Request-shape markers both real clients send on every call regardless of
	// transport (verified against the live gateway; only browser transport
	// headers like origin/referer differ per identity).
	req.Header.Set("x-requested-with", "XMLHttpRequest")
	req.Header.Set("x-codebuddy-request", "1")
	// SDK fingerprint family; see the consts above for why the values are pinned.
	req.Header.Set("x-stainless-lang", "js")
	req.Header.Set("x-stainless-package-version", stainlessPackageVersion)
	req.Header.Set("x-stainless-os", "MacOS")
	req.Header.Set("x-stainless-arch", "x64")
	req.Header.Set("x-stainless-runtime", "node")
	req.Header.Set("x-stainless-runtime-version", stainlessRuntimeVersion)
	req.Header.Set("x-stainless-retry-count", "0")
	req.Header.Set("x-stainless-timeout", "1200")
	if tok := deviceTokenFor(sa); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
	// The upstream is always streamed by this plugin; say so on the wire.
	req.Header.Set("Accept", "text/event-stream")
}

// applyConversationHeaders stamps the conversation/tracing family.
//
// `X-Conversation-ID` is cache identity — stable per conversation, which is
// what makes prompt caching hit. The rest are correlation ids: the upstream's
// channel check reads the turn-level `X-Conversation-Request-ID` and refuses a
// chat call that arrives without it (`11128 "conversation id missing"`), and
// the B3 pair mirrors what an official client reports. Ids are lowercase hex
// with the exact lengths the B3 spec allows (trace 32, span 16).
func applyConversationHeaders(req *http.Request, cid string) (turnID, messageID string) {
	if cid == "" {
		cid = randomHex(16)
	}
	turnID = randomHex(16)    // 32 hex chars: turn-scoped aggregation key
	messageID = randomHex(16) // 32 hex chars: message-scoped id

	req.Header.Set("X-Conversation-ID", cid)
	req.Header.Set("X-Conversation-Request-ID", turnID)
	req.Header.Set("X-Root-Request-ID", turnID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("x-trace-id", turnID)
	req.Header.Set("X-B3-TraceId", messageID)
	req.Header.Set("X-B3-SpanId", messageID[:16])
	req.Header.Set("X-B3-Sampled", "1")
	return turnID, messageID
}
