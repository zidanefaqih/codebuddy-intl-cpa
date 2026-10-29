package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// stubClientVersionRefresh disables the background feed fetch for a test and
// restores the real resolver afterwards, so tests never touch the network.
func stubClientVersionRefresh(t *testing.T) {
	t.Helper()
	prev := clientVersionRefresh
	clientVersionRefresh = func() {}
	t.Cleanup(func() { clientVersionRefresh = prev })
}

func resetClientVersionCache(t *testing.T) {
	t.Helper()
	clientVersionMu.Lock()
	prevValue, prevAt := clientVersionValue, clientVersionFetchedAt
	clientVersionValue, clientVersionFetchedAt = "", timeZero()
	clientVersionMu.Unlock()
	t.Cleanup(func() {
		clientVersionMu.Lock()
		clientVersionValue, clientVersionFetchedAt = prevValue, prevAt
		clientVersionMu.Unlock()
	})
}

func timeZero() time.Time { return time.Time{} }

func TestIdentityForRealm(t *testing.T) {
	cases := []struct {
		name   string
		domain string
		want   string
	}{
		{"intl codebuddy host", "www.codebuddy.ai", "IDE"},
		{"intl workbuddy host with scheme and path", "https://www.workbuddy.ai/", "IDE"},
		{"intl bare domain", "codebuddy.ai", "IDE"},
		{"cn domain", "www.codebuddy.cn", "CLI"},
		{"cn gateway", "copilot.tencent.com", "CLI"},
		{"empty domain stays legacy cn", "", "CLI"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sa := &storedAuth{}
			sa.Auth.Domain = tc.domain
			if got := identityFor(sa); got != tc.want {
				t.Errorf("identityFor(%q) = %q, want %q", tc.domain, got, tc.want)
			}
		})
	}
	if got := identityFor(nil); got != "CLI" {
		t.Errorf("identityFor(nil) = %q, want CLI", got)
	}
}

func TestBuildClientUA(t *testing.T) {
	if got, want := buildClientUA("IDE", "2.168.2"), "IDE/2.168.2 CodeBuddy/2.168.2"; got != want {
		t.Errorf("buildClientUA = %q, want %q", got, want)
	}
	if got, want := buildClientUA("CLI", "2.63.2"), "CLI/2.63.2 CodeBuddy/2.63.2"; got != want {
		t.Errorf("buildClientUA = %q, want %q", got, want)
	}
}

func TestClientVersionEnvOverrideWins(t *testing.T) {
	stubClientVersionRefresh(t)
	resetClientVersionCache(t)
	t.Setenv("WB_CLIENT_VERSION", "9.9.9")
	t.Setenv("WB_CLIENT_VERSION_INTL", "8.8.8")
	if got := clientVersionFor(true); got != "8.8.8" {
		t.Errorf("intl override = %q, want 8.8.8", got)
	}
	if got := clientVersionFor(false); got != "9.9.9" {
		t.Errorf("cn override = %q, want 9.9.9", got)
	}
}

func TestClientVersionFallsBackWithoutCache(t *testing.T) {
	stubClientVersionRefresh(t)
	resetClientVersionCache(t)
	t.Setenv("WB_CLIENT_VERSION", "")
	t.Setenv("WB_CLIENT_VERSION_INTL", "")
	t.Setenv("WB_CLIENT_VERSION_CN", "")
	if got := clientVersionFor(true); got != fallbackIntlClientVersion {
		t.Errorf("intl fallback = %q, want %q", got, fallbackIntlClientVersion)
	}
	if got := clientVersionFor(false); got != fallbackCNClientVersion {
		t.Errorf("cn fallback = %q, want %q", got, fallbackCNClientVersion)
	}
}

func TestSemverish(t *testing.T) {
	for _, v := range []string{"2.168.2", "1.0", "4.12.2.41207131"} {
		if !semverish(v) {
			t.Errorf("semverish(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "v2.1.0", "latest", "2.x.1", "<html>"} {
		if semverish(v) {
			t.Errorf("semverish(%q) = true, want false", v)
		}
	}
}

func TestApplyIdentityHeadersPerRealm(t *testing.T) {
	stubClientVersionRefresh(t)
	resetClientVersionCache(t)
	t.Setenv("WB_CLIENT_VERSION", "")

	intl := &storedAuth{}
	intl.Auth.Domain = "www.codebuddy.ai"
	req, _ := http.NewRequest(http.MethodPost, "https://www.codebuddy.ai/v2/chat/completions", nil)
	applyIdentityHeaders(req, intl)
	if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "IDE/") || !strings.Contains(got, "CodeBuddy/") {
		t.Errorf("intl User-Agent = %q, want IDE/<version> CodeBuddy/<version>", got)
	}
	if got := req.Header.Get("X-IDE-Type"); got != "IDE" {
		t.Errorf("intl X-IDE-Type = %q, want IDE", got)
	}
	if got := req.Header.Get("X-IDE-Name"); got != "IDE" {
		t.Errorf("intl X-IDE-Name = %q, want IDE", got)
	}
	if got := req.Header.Get("x-codebuddy-request"); got != "1" {
		t.Errorf("intl x-codebuddy-request = %q, want 1", got)
	}
	if got := req.Header.Get("x-requested-with"); got != "XMLHttpRequest" {
		t.Errorf("x-requested-with = %q, want XMLHttpRequest on every identity", got)
	}
	for h, want := range map[string]string{
		"x-stainless-lang":            "js",
		"x-stainless-package-version": stainlessPackageVersion,
		"x-stainless-os":              "MacOS",
		"x-stainless-arch":            "x64",
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": stainlessRuntimeVersion,
		"x-stainless-retry-count":     "0",
		"x-stainless-timeout":         "1200",
	} {
		if got := req.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q (pinned SDK fingerprint)", h, got, want)
		}
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", got)
	}
	if got := req.Header.Get("X-Device-Token"); got != "" {
		t.Errorf("X-Device-Token = %q, want omitted when the account has none", got)
	}

	cn := &storedAuth{}
	cn.Auth.Domain = "www.codebuddy.cn"
	reqCN, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	applyIdentityHeaders(reqCN, cn)
	if got := reqCN.Header.Get("User-Agent"); !strings.HasPrefix(got, "CLI/") {
		t.Errorf("cn User-Agent = %q, want CLI/<version> CodeBuddy/<version>", got)
	}
	if got := reqCN.Header.Get("X-IDE-Type"); got != "CLI" {
		t.Errorf("cn X-IDE-Type = %q, want CLI", got)
	}
	if got := reqCN.Header.Get("x-stainless-os"); got != "MacOS" {
		t.Errorf("cn x-stainless-os = %q, want the pinned fingerprint, not the host OS", got)
	}
	if got := reqCN.Header.Get("x-requested-with"); got != "XMLHttpRequest" {
		t.Errorf("cn x-requested-with = %q, want XMLHttpRequest", got)
	}
}

func TestApplyConversationHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://www.codebuddy.ai/v2/chat/completions", nil)
	turnID, messageID := applyConversationHeaders(req, "stable-conversation")

	if got := req.Header.Get("X-Conversation-ID"); got != "stable-conversation" {
		t.Errorf("X-Conversation-ID = %q, want the stable conversation id", got)
	}
	if got := req.Header.Get("X-Conversation-Request-ID"); got != turnID || got == "" {
		t.Errorf("X-Conversation-Request-ID = %q, want %q", got, turnID)
	}
	if got := req.Header.Get("X-Root-Request-ID"); got != turnID {
		t.Errorf("X-Root-Request-ID = %q, want the turn id %q", got, turnID)
	}
	if got := req.Header.Get("X-Conversation-Message-ID"); got != messageID || got == "" {
		t.Errorf("X-Conversation-Message-ID = %q, want %q", got, messageID)
	}
	if got := req.Header.Get("X-Request-ID"); got != messageID {
		t.Errorf("X-Request-ID = %q, want the message id %q", got, messageID)
	}
	// B3: trace must be 32 hex and span 16 hex, or the upstream drops the pair.
	trace := req.Header.Get("X-B3-TraceId")
	span := req.Header.Get("X-B3-SpanId")
	if len(trace) != 32 || !isLowerHex(trace) {
		t.Errorf("X-B3-TraceId = %q, want 32 lowercase hex chars", trace)
	}
	if len(span) != 16 || !isLowerHex(span) {
		t.Errorf("X-B3-SpanId = %q, want 16 lowercase hex chars", span)
	}
	if trace != messageID || span != messageID[:16] {
		t.Errorf("B3 pair must derive from the message id: trace=%q span=%q message=%q", trace, span, messageID)
	}
	if got := req.Header.Get("X-B3-Sampled"); got != "1" {
		t.Errorf("X-B3-Sampled = %q, want 1", got)
	}
	if got := req.Header.Get("x-trace-id"); got != turnID {
		t.Errorf("x-trace-id = %q, want the turn id %q", got, turnID)
	}

	// An empty conversation id must still produce a usable family.
	reqNoCID, _ := http.NewRequest(http.MethodPost, "https://www.codebuddy.ai/v2/chat/completions", nil)
	turnID2, messageID2 := applyConversationHeaders(reqNoCID, "")
	if turnID2 == "" || messageID2 == "" {
		t.Error("empty conversation id must still yield fresh turn/message ids")
	}
	if got := reqNoCID.Header.Get("X-Conversation-ID"); len(got) != 32 {
		t.Errorf("generated X-Conversation-ID = %q, want 32 hex chars", got)
	}
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return s != ""
}

func TestDeviceTokenFor(t *testing.T) {
	t.Setenv("WB_DEVICE_TOKEN", "")
	sa := &storedAuth{}
	if got := deviceTokenFor(sa); got != "" {
		t.Errorf("deviceTokenFor(empty) = %q, want empty", got)
	}
	if got := deviceTokenFor(nil); got != "" {
		t.Errorf("deviceTokenFor(nil) = %q, want empty", got)
	}

	sa.rawJSON = []byte(`{"auth":{"accessToken":"x","deviceToken":"dt-abc"}}`)
	if got := deviceTokenFor(sa); got != "dt-abc" {
		t.Errorf("deviceTokenFor(auth.deviceToken) = %q, want dt-abc", got)
	}
	sa.rawJSON = []byte(`{"device_token":"dt-snake"}`)
	if got := deviceTokenFor(sa); got != "dt-snake" {
		t.Errorf("deviceTokenFor(device_token) = %q, want dt-snake", got)
	}
	sa.rawJSON = []byte(`not json`)
	if got := deviceTokenFor(sa); got != "" {
		t.Errorf("deviceTokenFor(garbage) = %q, want empty", got)
	}

	t.Setenv("WB_DEVICE_TOKEN", "dt-env")
	sa.rawJSON = []byte(`{"auth":{"deviceToken":"dt-abc"}}`)
	if got := deviceTokenFor(sa); got != "dt-env" {
		t.Errorf("deviceTokenFor(env override) = %q, want dt-env", got)
	}
}
