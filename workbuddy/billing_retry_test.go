package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestBillingCall_RetriesOn5xx verifies that a transient upstream 500 is
// retried and ultimately succeeds when the next attempt returns 200.
func TestBillingCall_RetriesOn5xx(t *testing.T) {
	orig := billingRetryDelays
	billingRetryDelays = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond}
	defer func() { billingRetryDelays = orig }()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 { // first two attempts → 500
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"k":"v"}}`))
	}))
	defer srv.Close()

	// Temporarily override billingBase so the test server is used.
	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	data, err := billingCall(sa, "/test", nil)
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if string(data) != `{"k":"v"}` {
		t.Fatalf("unexpected data: %s", string(data))
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls (2 retry), got %d", calls)
	}
}

// TestBillingCall_NoRetryOn4xx verifies that business-level errors (4xx,
// non-zero code) are not retried.
func TestBillingCall_NoRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"msg":"bad request"}`))
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	_, err := billingCall(sa, "/test", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Should not retry on 4xx — exactly 1 call.
	if calls != 1 {
		t.Fatalf("expected 1 call (no retry on 4xx), got %d", calls)
	}
}

// TestIsTransientBillingErr covers classification boundaries.
func TestMutatingBillingCallsDoNotRetry5xx(t *testing.T) {
	orig := billingRetryDelays
	billingRetryDelays = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond}
	defer func() { billingRetryDelays = orig }()

	var checkinCalls, trialCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var calls *int32
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			calls = &checkinCalls
		case "/billing/ide/trial":
			calls = &trialCalls
		default:
			t.Fatalf("unexpected mutation path %s", r.URL.Path)
		}
		atomic.AddInt32(calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"transient"}`))
	}))
	defer srv.Close()

	globalAuth := &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}
	cnAuth := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}
	restoreGlobal := setBillingBaseGlobal(srv.URL)
	if _, err := performTrialCall(globalAuth); err != nil {
		t.Fatalf("performTrialCall returned transport error: %v", err)
	}
	restoreGlobal()
	restoreCN := setBillingBase(srv.URL)
	if _, err := performCheckinCall(cnAuth); err != nil {
		t.Fatalf("performCheckinCall returned transport error: %v", err)
	}
	restoreCN()
	if got := atomic.LoadInt32(&trialCalls); got != 1 {
		t.Fatalf("trial POST count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&checkinCalls); got != 1 {
		t.Fatalf("check-in POST count = %d, want 1", got)
	}
}

func TestMutatingBillingCallsDoNotRetryTransportFailure(t *testing.T) {
	var checkinCalls, trialCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var calls *int32
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			calls = &checkinCalls
		case "/billing/ide/trial":
			calls = &trialCalls
		default:
			t.Errorf("unexpected mutation path %s", r.URL.Path)
			return
		}
		atomic.AddInt32(calls, 1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("response writer does not support hijacking")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	globalAuth := &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}
	cnAuth := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}
	restoreGlobal := setBillingBaseGlobal(srv.URL)
	if _, err := performTrialCall(globalAuth); err != nil {
		t.Fatalf("performTrialCall returned transport error: %v", err)
	}
	restoreGlobal()
	restoreCN := setBillingBase(srv.URL)
	if _, err := performCheckinCall(cnAuth); err != nil {
		t.Fatalf("performCheckinCall returned transport error: %v", err)
	}
	restoreCN()
	if got := atomic.LoadInt32(&trialCalls); got != 1 {
		t.Fatalf("trial POST count after transport failure = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&checkinCalls); got != 1 {
		t.Fatalf("check-in POST count after transport failure = %d, want 1", got)
	}
}

func TestIsTransientBillingErr(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("http 500 from /v2/billing: internal"), true},
		{errors.New("http 503 from /v2/billing: unavailable"), true},
		{errors.New("dial tcp: connection reset by peer"), false},
		{errors.New("code=10000 msg=API request failed"), false}, // business code, not transient
		{errors.New("parse failed: unexpected EOF"), false},
	}
	for _, tt := range tests {
		if got := isTransientBillingErr(tt.err); got != tt.want {
			t.Errorf("isTransientBillingErr(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
