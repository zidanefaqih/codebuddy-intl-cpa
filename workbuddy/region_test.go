package main

import "testing"

func TestIsGlobalDomain(t *testing.T) {
	cases := []struct {
		domain string
		want   bool
	}{
		{"www.workbuddy.ai", true},
		{"workbuddy.ai", true},
		{"https://www.codebuddy.ai", true},
		{"https://www.codebuddy.ai/console", true},
		{"www.codebuddy.ai:443", true},
		{"www.codebuddy.cn", false},
		{"", false},
		{"WORKBUDDY.AI", true},
		{"  www.workbuddy.ai  ", true},
		// A-33: substring match was too loose
		{"evilworkbuddy.ai", false},
		{"workbuddy.ai.evil.com", false},
		{"notworkbuddy.ai", false},
		{"https://workbuddy.ai.evil.com", false},
	}
	for _, tc := range cases {
		if got := isGlobalDomain(tc.domain); got != tc.want {
			t.Errorf("isGlobalDomain(%q) = %v, want %v", tc.domain, got, tc.want)
		}
	}
}

func TestAccountRegion(t *testing.T) {
	cases := []struct {
		name   string
		sa     *storedAuth
		region string
	}{
		{"nil", nil, "cn"},
		{"empty domain", &storedAuth{}, "cn"},
		{"CN domain", &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}, "cn"},
		{"Global domain", &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}, "global"},
	}
	for _, tc := range cases {
		if got := accountRegion(tc.sa); got != tc.region {
			t.Errorf("%s: accountRegion() = %q, want %q", tc.name, got, tc.region)
		}
	}
}

func TestBillingBaseFor(t *testing.T) {
	cases := []struct {
		name string
		sa   *storedAuth
		want string
	}{
		{"nil", nil, billingBase},
		{"empty domain", &storedAuth{}, billingBase},
		{"CN domain", &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}, billingBase},
		{"Global domain", &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}, billingBaseGlobal},
	}
	for _, tc := range cases {
		if got := billingBaseFor(tc.sa); got != tc.want {
			t.Errorf("%s: billingBaseFor() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestOriginRefererFor(t *testing.T) {
	// NOTE (2026-09-29): the Global expectation here used to be
	// "https://www.workbuddy.ai". The working tree routes BOTH Global issuers to a
	// single Global gateway host (originRefererGlobal = www.codebuddy.ai, matching
	// upstreamBaseGlobal), so the assertion now follows the implementation.
	//
	// Evidence for the single-host choice: a workbuddy.ai-domain credential answers
	// on both www.workbuddy.ai and www.codebuddy.ai (verified 2026-09-28 against the
	// billing and growth endpoints). If that choice is ever reverted, revert this
	// case too — the two must stay consistent, or the Origin/Referer no longer
	// matches the host the request actually goes to.
	cases := []struct {
		name string
		sa   *storedAuth
		want string
	}{
		{"nil", nil, originReferer},
		{"empty domain", &storedAuth{}, originReferer},
		{"CN domain", &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}, originReferer},
		{"Global domain (workbuddy.ai)", &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}, originRefererGlobal},
		{"Global domain (codebuddy.ai)", &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.ai"}}, originRefererGlobal},
	}
	for _, tc := range cases {
		if got := originRefererFor(tc.sa); got != tc.want {
			t.Errorf("%s: originRefererFor() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
