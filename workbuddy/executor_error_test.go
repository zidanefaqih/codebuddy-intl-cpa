package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

type executorErrorTransport func(*http.Request) (*http.Response, error)

func (f executorErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestExecutorUpstreamErrorsPreserveHTTPStatus(t *testing.T) {
	client := sharedHTTPClient()
	original := client.Transport
	defer func() { client.Transport = original }()
	const body = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`
	calls := 0
	client.Transport = executorErrorTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	cand := &failoverCandidate{AuthID: "test", Auth: &storedAuth{}}
	payload := []byte(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[]}`)
	paths := []struct {
		name string
		run  func() error
	}{
		{"non-stream", func() error {
			_, err := upstreamAttempt(context.Background(), payload, "deepseek-v4.1-flash", cand)
			return err
		}},
		{"collected-stream", func() error {
			_, _, err := collectStreamAttempt(payload, cand, true, &sseUsageCollector{})
			return err
		}},
		{"async-stream", func() error {
			req, err := http.NewRequest(http.MethodPost, endpointChatFor(cand.Auth), strings.NewReader(string(payload)))
			if err != nil {
				return err
			}
			_, _, err = pumpAttempt(req, "test", true, "deepseek-v4.1-flash", "deepseek-v4.1-flash", "test", "test", time.Now(), &sseUsageCollector{})
			return err
		}},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			err := path.run()
			var upstream *upstreamStatusError
			if !errors.As(err, &upstream) || upstream.StatusCode() != http.StatusBadRequest || upstream.body != body {
				t.Fatalf("upstream error lost status/body: %v", err)
			}
			var wire pluginabi.Envelope
			if err := json.Unmarshal(errorEnvelopeFromError(fmt.Errorf("executor: %w", err)), &wire); err != nil {
				t.Fatal(err)
			}
			if wire.OK || wire.Error == nil || wire.Error.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("host error lost HTTP status: %+v", wire)
			}
			if !strings.Contains(wire.Error.Message, "11128") {
				t.Fatalf("host error lost upstream rejection: %s", wire.Error.Message)
			}
		})
	}
	if calls != len(paths) {
		t.Fatalf("made %d upstream calls, want %d", calls, len(paths))
	}
}

func TestErrorEnvelopeFromErrorRedactsSecrets(t *testing.T) {
	var wire pluginabi.Envelope
	err := errors.New("transport failure: Bearer secret-access-token")
	if err := json.Unmarshal(errorEnvelopeFromError(err), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.OK || wire.Error == nil || wire.Error.HTTPStatus != 0 {
		t.Fatalf("unexpected transport error: %+v", wire)
	}
	if strings.Contains(wire.Error.Message, "secret-access-token") {
		t.Fatal("error envelope exposed a credential")
	}
}

func TestStreamErrorsUseHostErrorField(t *testing.T) {
	old := streamErrorEmitCall
	t.Cleanup(func() { streamErrorEmitCall = old })
	calls := 0
	streamErrorEmitCall = func(method string, raw []byte) ([]byte, error) {
		calls++
		if method != pluginabi.MethodHostStreamEmit {
			t.Fatalf("unexpected host method: %s", method)
		}
		var wire struct {
			StreamID string `json:"stream_id"`
			Error    string `json:"error"`
			Payload  []byte `json:"payload"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if wire.StreamID != "fixture" || wire.Error == "" || len(wire.Payload) != 0 || strings.Contains(wire.Error, "secret-access-token") {
			t.Fatalf("incorrect stream error envelope: %+v", wire)
		}
		return nil, nil
	}
	streamEmitError("", "ignored")
	streamEmitError("fixture", "upstream failure: Bearer secret-access-token")
	if calls != 1 {
		t.Fatalf("host calls=%d, want 1", calls)
	}
}
