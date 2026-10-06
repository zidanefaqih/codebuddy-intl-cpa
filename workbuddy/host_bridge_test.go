package main

import (
	"strings"
	"testing"
)

func TestHostBridgeUnwrapPropagatesEnvelopeError(t *testing.T) {
	_, err := hostBridgeUnwrap([]byte(`{"ok":false,"error":{"code":"host_call_failed","message":"upstream unavailable"}}`), "host.http.do")
	if err == nil {
		t.Fatal("expected host callback error")
	}
	if !strings.Contains(err.Error(), "host error host_call_failed: upstream unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHostBridgeUnwrapRejectsMalformedEnvelope(t *testing.T) {
	_, err := hostBridgeUnwrap([]byte(`{"ok":`), "host.http.do_stream")
	if err == nil || !strings.Contains(err.Error(), "decode envelope") {
		t.Fatalf("expected malformed-envelope error, got %v", err)
	}
}

func TestHostBridgeUnwrapKeepsEmptySuccessfulResultAsErrorForDecoder(t *testing.T) {
	result, err := hostBridgeUnwrap([]byte(`{"ok":true}`), "host.http.do")
	if err != nil {
		t.Fatalf("unexpected unwrap error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("result = %s, want empty result", result)
	}
}

func TestHostHTTPBridgeUnavailableIsNarrow(t *testing.T) {
	if !hostHTTPBridgeUnavailable(assertError("unsupported host callback host.http.do")) {
		t.Fatal("unsupported callback should allow compatibility fallback")
	}
	if hostHTTPBridgeUnavailable(assertError("host http stream bridge is unavailable")) {
		t.Fatal("stream bridge failure must not replay the request")
	}
	if hostHTTPBridgeUnavailable(assertError("host error host_call_failed: upstream unavailable")) {
		t.Fatal("real callback failure must not trigger direct replay")
	}
}

type testError string

func (e testError) Error() string { return string(e) }

func assertError(message string) error { return testError(message) }
