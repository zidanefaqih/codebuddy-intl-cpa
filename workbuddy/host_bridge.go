// host_bridge.go routes every upstream HTTP call through the CPA host's
// http bridge (host.http.do / host.http.do_stream / stream_read / stream_close).
// Production traffic always uses the bridge so request-log captures outbound
// calls and host transport policy (proxy, timeout) applies. The *Direct
// variants are the test-only fallback used when the bridge is unavailable
// (unit tests, or hosts older than v7.2.x without the http bridge RPC).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// sharedHTTPClient is the fallback HTTP client used ONLY when the host HTTP
// bridge is unavailable (unit tests, or hosts older than v7.2.x without
// host.http.* RPC). All production upstream calls should route via hostHTTPDo
// / hostHTTPDoStream so request-log captures them and host transport policy
// applies. Direct use of this client in new code is a compliance bug.
func sharedHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		// No cookie jar here: auth is carried by Bearer headers, and a shared
		// jar would leak upstream set-cookie state across accounts (multi-account
		// deployments could cross-contaminate sessions). Only the short-lived
		// login clients get a jar.
		sharedClient = &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 5,
			},
		}
	})
	return sharedClient
}

// hostHTTPResponse is the plugin-side view of an HTTP response that came back
// through the host bridge. Body is fully buffered (matches the historical
// io.ReadAll(resp.Body) usage pattern in billing / models / usage callers).
type hostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// rpcHostHTTPRequestWire mirrors internal/pluginhost/host_callbacks.go's
// rpcHostHTTPRequest on the wire. The "request" sub-object is the actual HTTP
// call; the flat method/url/headers/body fields are an alternate form we don't
// use (host prefers Request when present).
type rpcHostHTTPRequestWire struct {
	Request *rpcHostHTTPInner `json:"request,omitempty"`
}

type rpcHostHTTPInner struct {
	Method  string              `json:"method,omitempty"`
	URL     string              `json:"url,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// rpcHostHTTPResponseWire accepts both the snake_case wire format used by
// newer hosts and the Go field-name format emitted by older CPA runtimes.
// pluginapi.HTTPResponse has no JSON tags, so older hosts serialize
// StatusCode/Headers/Body instead of status_code/headers/body.
type rpcHostHTTPResponseWire struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

func (r *rpcHostHTTPResponseWire) UnmarshalJSON(data []byte) error {
	var wire struct {
		StatusCode       int                 `json:"status_code"`
		LegacyStatusCode int                 `json:"StatusCode"`
		CamelStatusCode  int                 `json:"statusCode"`
		Headers          map[string][]string `json:"headers"`
		LegacyHeaders    map[string][]string `json:"Headers"`
		Body             []byte              `json:"body"`
		LegacyBody       []byte              `json:"Body"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	statusCode := wire.StatusCode
	if statusCode <= 0 {
		statusCode = wire.LegacyStatusCode
	}
	if statusCode <= 0 {
		statusCode = wire.CamelStatusCode
	}
	headers := wire.Headers
	if headers == nil {
		headers = wire.LegacyHeaders
	}
	body := wire.Body
	if body == nil {
		body = wire.LegacyBody
	}
	*r = rpcHostHTTPResponseWire{
		StatusCode: statusCode,
		Headers:    headers,
		Body:       body,
	}
	return nil
}

type rpcHostHTTPStreamResponseWire struct {
	StatusCode int
	Headers    map[string][]string
	StreamID   string
	Chunks     []pluginapi.HTTPStreamChunk
}

func (r *rpcHostHTTPStreamResponseWire) UnmarshalJSON(data []byte) error {
	var wire struct {
		StatusCode       int                         `json:"status_code"`
		LegacyStatusCode int                         `json:"StatusCode"`
		CamelStatusCode  int                         `json:"statusCode"`
		Headers          map[string][]string         `json:"headers"`
		LegacyHeaders    map[string][]string         `json:"Headers"`
		StreamID         string                      `json:"stream_id"`
		LegacyStreamID   string                      `json:"StreamID"`
		CamelStreamID    string                      `json:"streamId"`
		Chunks           []pluginapi.HTTPStreamChunk `json:"chunks"`
		LegacyChunks     []pluginapi.HTTPStreamChunk `json:"Chunks"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	statusCode := wire.StatusCode
	if statusCode <= 0 {
		statusCode = wire.LegacyStatusCode
	}
	if statusCode <= 0 {
		statusCode = wire.CamelStatusCode
	}
	headers := wire.Headers
	if headers == nil {
		headers = wire.LegacyHeaders
	}
	streamID := wire.StreamID
	if streamID == "" {
		streamID = wire.LegacyStreamID
	}
	if streamID == "" {
		streamID = wire.CamelStreamID
	}
	chunks := wire.Chunks
	if chunks == nil {
		chunks = wire.LegacyChunks
	}
	*r = rpcHostHTTPStreamResponseWire{
		StatusCode: statusCode,
		Headers:    headers,
		StreamID:   streamID,
		Chunks:     chunks,
	}
	return nil
}

type rpcHostHTTPStreamReadResponseWire struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// hostBridgeUnwrap unwraps the pluginabi.Envelope returned by host RPC and
// returns the inner Result payload. Returns an error when the envelope itself
// signals failure (ok=false) or is malformed.
func hostBridgeUnwrap(raw []byte, method string) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: decode envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: host error %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("%s: host returned not-ok", method)
	}
	return env.Result, nil
}

// hostBridgeAvailable reports whether host.http.* RPC is wired up. False in
// unit tests (no hostAPI) and when the host binary predates the bridge.
func hostBridgeAvailable() bool {
	return hostAPI != nil && hostAPI.call != nil
}

// hostHTTPDo performs a non-streaming upstream call via the host's http bridge.
// Request body is read eagerly (callers already have []byte or a small buffer).
// The response body is likewise read eagerly — all existing call sites consume
// it via io.ReadAll then Close, so we keep that shape and discard the closer.
//
// Fallback: when the host bridge is unavailable (unit tests, host older than
// v7.2.x without the http bridge), we route through sharedHTTPClient directly.
// This keeps the plugin functional in dev/test contexts while preferring the
// compliant path in production.
func hostHTTPDo(req *http.Request) (*hostHTTPResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		_ = req.Body.Close()
		bodyBytes = b
	}
	// Windows stack movement mitigation: nested host calls during synchronous
	// RPCs (model.for_auth, management.handle) cause the host stack to move,
	// rendering the stack response pointer dangling and causing "unexpected
	// end of JSON input". Bypass host bridge on Windows and make direct HTTP
	// calls.
	if !hostBridgeAvailable() || runtime.GOOS == "windows" {
		return hostHTTPDoDirect(req, bodyBytes)
	}
	wire := rpcHostHTTPRequestWire{
		Request: &rpcHostHTTPInner{
			Method:  req.Method,
			URL:     req.URL.String(),
			Headers: map[string][]string(req.Header),
			Body:    bodyBytes,
		},
	}
	raw, err := hostCall(pluginabi.MethodHostHTTPDo, mustJSON(wire))
	if err != nil {
		if hostHTTPBridgeUnavailable(err) {
			return hostHTTPDoDirect(req, bodyBytes)
		}
		return nil, err
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPDo)
	if err != nil {
		if hostHTTPBridgeUnavailable(err) {
			return hostHTTPDoDirect(req, bodyBytes)
		}
		return nil, err
	}
	var resp rpcHostHTTPResponseWire
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decode host.http.do response: %w", err)
	}
	if resp.StatusCode <= 0 {
		return nil, fmt.Errorf("%s: response missing status_code", pluginabi.MethodHostHTTPDo)
	}
	return &hostHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    http.Header(resp.Headers),
		Body:       resp.Body,
	}, nil
}

// hostHTTPDoDirect executes the request via the plugin's own http.Client.
// Used as a fallback when the host bridge is unavailable (unit tests).
func hostHTTPDoDirect(req *http.Request, bodyBytes []byte) (*hostHTTPResponse, error) {
	// Rebuild the request since req.Body was already consumed.
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header.Clone()
	resp, err := sharedHTTPClient().Do(newReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &hostHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Body:       raw,
	}, nil
}

// hostHTTPStream is a handle for an in-flight host-bridged stream. Read returns
// the next chunk; Close aborts the upstream.
//
// Two modes:
//   - Bridged: streamID set, Read/Close forward to host RPC.
//   - Direct (test fallback): direct holds the full buffered body, Read drains
//     it once then reports done. Close is a no-op.
type hostHTTPStream struct {
	streamID string
	direct   []byte
	directAt int
}

// hostHTTPDoStream opens a streaming call via the host bridge. The host owns
// the actual http.Response body; we pull chunks via hostHTTPStreamRead.
//
// Falls back to direct http.Client.Do when the bridge is unavailable (tests).
// In that case the returned hostHTTPStream wraps an in-memory copy of the
// full response body so Read/Close have the same shape.
func hostHTTPDoStream(req *http.Request) (*hostHTTPStream, int, http.Header, error) {
	if req == nil {
		return nil, 0, nil, fmt.Errorf("nil request")
	}
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("read request body: %w", err)
		}
		_ = req.Body.Close()
		bodyBytes = b
	}
	// Match hostHTTPDo's Windows mitigation: nested synchronous callbacks can
	// invalidate the C stack response pointer on the host's Windows loader.
	if !hostBridgeAvailable() || runtime.GOOS == "windows" {
		return hostHTTPDoStreamDirect(req, bodyBytes)
	}
	wire := rpcHostHTTPRequestWire{
		Request: &rpcHostHTTPInner{
			Method:  req.Method,
			URL:     req.URL.String(),
			Headers: map[string][]string(req.Header),
			Body:    bodyBytes,
		},
	}
	raw, err := hostCall(pluginabi.MethodHostHTTPDoStream, mustJSON(wire))
	if err != nil {
		if hostHTTPBridgeUnavailable(err) {
			return hostHTTPDoStreamDirect(req, bodyBytes)
		}
		return nil, 0, nil, err
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPDoStream)
	if err != nil {
		if hostHTTPBridgeUnavailable(err) {
			return hostHTTPDoStreamDirect(req, bodyBytes)
		}
		return nil, 0, nil, err
	}
	var resp rpcHostHTTPStreamResponseWire
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, 0, nil, fmt.Errorf("decode host.http.do_stream response: %w", err)
	}
	if resp.StreamID == "" {
		return nil, resp.StatusCode, http.Header(resp.Headers), fmt.Errorf("host http stream bridge is unavailable")
	}
	return &hostHTTPStream{streamID: resp.StreamID}, resp.StatusCode, http.Header(resp.Headers), nil
}

// hostHTTPDoStreamDirect is the test-only fallback: it performs the request
// with the plugin's own http.Client and buffers the full body into an
// in-memory hostHTTPStream so Read/Close keep the same contract.
func hostHTTPDoStreamDirect(req *http.Request, bodyBytes []byte) (*hostHTTPStream, int, http.Header, error) {
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, nil, err
	}
	newReq.Header = req.Header.Clone()
	resp, err := sharedHTTPClient().Do(newReq)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Clone(), err
	}
	return &hostHTTPStream{direct: raw}, resp.StatusCode, resp.Header.Clone(), nil
}

// Read pulls the next chunk. Returns (payload, done, err). done=true means the
// stream ended cleanly; err non-nil means upstream or bridge error.
func (s *hostHTTPStream) Read() ([]byte, bool, error) {
	if s == nil {
		return nil, true, fmt.Errorf("stream closed")
	}
	// Direct (test fallback) mode: serve the buffered body in one shot.
	if s.direct != nil {
		if s.directAt >= len(s.direct) {
			return nil, true, nil
		}
		out := s.direct[s.directAt:]
		s.directAt = len(s.direct)
		return out, false, nil
	}
	if s.streamID == "" {
		return nil, true, fmt.Errorf("stream closed")
	}
	raw, err := hostCall(pluginabi.MethodHostHTTPStreamRead, mustJSON(map[string]any{"stream_id": s.streamID}))
	if err != nil {
		return nil, true, err
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPStreamRead)
	if err != nil {
		return nil, true, err
	}
	var resp rpcHostHTTPStreamReadResponseWire
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, true, fmt.Errorf("decode host.http.stream_read response: %w", err)
	}
	if resp.Error != "" {
		return nil, true, fmt.Errorf("%s", resp.Error)
	}
	return resp.Payload, resp.Done, nil
}

// Close aborts the upstream stream. Always safe to call (idempotent on host).
func (s *hostHTTPStream) Close() {
	if s == nil {
		return
	}
	if s.direct != nil {
		s.direct = nil
		s.directAt = 0
		return
	}
	if s.streamID == "" {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostHTTPStreamClose, mustJSON(map[string]any{"stream_id": s.streamID}))
	s.streamID = ""
}

// hostStreamReader adapts a hostHTTPStream to io.Reader so existing
// bufio.Scanner / io.ReadAll call sites work unchanged. The host bridge emits
// arbitrary 32KB chunks (not SSE lines), so line framing must be re-assembled
// by the consumer — Scanner handles that for us.
type hostStreamReader struct {
	s    *hostHTTPStream
	buf  []byte // leftover from previous chunk
	done bool
	err  error
}

func newHostStreamReader(s *hostHTTPStream) *hostStreamReader {
	return &hostStreamReader{s: s}
}

func (r *hostStreamReader) Read(p []byte) (int, error) {
	for {
		// Drain buffered bytes first.
		if len(r.buf) > 0 {
			n := copy(p, r.buf)
			r.buf = r.buf[n:]
			return n, nil
		}
		if r.done {
			if r.err != nil {
				return 0, r.err
			}
			return 0, io.EOF
		}
		chunk, done, err := r.s.Read()
		if err != nil {
			r.done = true
			r.err = err
			return 0, err
		}
		if len(chunk) > 0 {
			n := copy(p, chunk)
			if n < len(chunk) {
				r.buf = append(r.buf, chunk[n:]...)
			}
			if done {
				r.done = true
			}
			return n, nil
		}
		if done {
			r.done = true
			return 0, io.EOF
		}
		// Empty chunk, not done: keep pulling without growing the call stack.
	}
}

// hostHTTPBridgeUnavailable is intentionally limited to an unsupported
// callback. Other bridge errors may happen after the host has sent the
// upstream request; retrying the POST via the plugin's direct client could
// duplicate billing or a state mutation.
func hostHTTPBridgeUnavailable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "unsupported host callback")
}

// mustJSON marshals v and panics on error — the wire structs above are always
// marshalable, so any failure here is a programming bug.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
