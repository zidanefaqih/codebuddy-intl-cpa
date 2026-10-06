package main

import (
	"encoding/json"
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

func TestHostHTTPResponseWireAcceptsCPAFieldStyles(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		status int
		header string
		body   string
	}{
		{
			name:   "snake_case",
			raw:    `{"status_code":204,"headers":{"X-Test":["snake"]},"body":"c25ha2U="}`,
			status: 204,
			header: "snake",
			body:   "snake",
		},
		{
			name:   "legacy_go_fields",
			raw:    `{"StatusCode":206,"Headers":{"X-Test":["legacy"]},"Body":"bGVnYWN5"}`,
			status: 206,
			header: "legacy",
			body:   "legacy",
		},
		{
			name:   "camel_case",
			raw:    `{"statusCode":207,"headers":{"X-Test":["camel"]},"body":"Y2FtZWw="}`,
			status: 207,
			header: "camel",
			body:   "camel",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got rpcHostHTTPResponseWire
			if err := json.Unmarshal([]byte(tt.raw), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", got.StatusCode, tt.status)
			}
			if got.Headers["X-Test"][0] != tt.header {
				t.Fatalf("header = %#v, want %q", got.Headers["X-Test"], tt.header)
			}
			if string(got.Body) != tt.body {
				t.Fatalf("body = %q, want %q", got.Body, tt.body)
			}
		})
	}
}

func TestHostHTTPStreamResponseWireAcceptsLegacyFields(t *testing.T) {
	var got rpcHostHTTPStreamResponseWire
	raw := []byte(`{"StatusCode":200,"Headers":{"X-Test":["legacy"]},"StreamID":"stream-legacy"}`)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", got.StatusCode)
	}
	if got.Headers["X-Test"][0] != "legacy" {
		t.Fatalf("headers = %#v, want legacy header", got.Headers)
	}
	if got.StreamID != "stream-legacy" {
		t.Fatalf("stream ID = %q, want %q", got.StreamID, "stream-legacy")
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
