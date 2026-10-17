// stream.go owns the upstream SSE data plane: emitting cleaned chunks back to
// the host stream (streamEmit/close), pumping the upstream SSE in a goroutine
// with 429 failover (pumpWithFailover), collecting it synchronously
// (collectStreamAttempt in main.go), and the SSE-frame helpers that re-frame,
// filter, and aggregate chunks.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

// Separate the error callback so tests can inspect the host bridge envelope.
var streamErrorEmitCall = hostCall

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	// Errors must use the bridge's error field; cross-format translators can
	// discard a JSON error payload because it has no completion choices.
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "error": redactSecrets(message)})
	_, _ = streamErrorEmitCall(pluginabi.MethodHostStreamEmit, body)
}

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID})
	_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpWithFailover is the async-stream pump wrapped in the 429 failover loop
// (v0.8.8). Each eligible account is tried in order; a rate limit observed
// BEFORE the first chunk is emitted marks the account and moves to the next.
// Once any chunk reached the client the stream is committed and never
// replayed. Usage is recorded once per inbound request: success on the
// answering account, one failure line when every attempt was rate-limited.
func pumpWithFailover(ctx context.Context, cancel context.CancelFunc, req executorStreamRequest, body []byte, upstreamModel, authUID string, started time.Time, sseFramed bool, cands []*failoverCandidate, traces ...*requestCacheTrace) {
	trace := firstCacheTrace(traces)
	if trace == nil {
		trace = newCacheTrace(req.Payload, req.OriginalRequest, body, req.Metadata, started)
	}
	if cancel != nil {
		defer cancel()
	}
	streamID := req.StreamID
	closed := false
	closeOnce := func() {
		if closed {
			return
		}
		closed = true
		streamClose(streamID)
	}
	defer closeOnce()

	collector := &sseUsageCollector{}
	realm := mmPrefixRealm(req.Model)
	if sa, err := parseStored(req.StorageJSON); err == nil {
		realm = accountRegion(sa)
	}
	fin := runStreamFailoverForSession(cands, requestSessionKey(req.ExecutorRequest, body, realm), func(cand *failoverCandidate) (bool, int, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointChatFor(cand.Auth), bytes.NewReader(body))
		if err != nil {
			return false, 0, err
		}
		backendHeaders(httpReq, cand.Auth)
		trace.beginAttempt(httpReq, cand.AuthID)
		return pumpAttempt(httpReq, streamID, sseFramed, req.Model, upstreamModel, cand.AuthID, candUID(cand, authUID), started, collector, trace)
	}, ctx)

	switch fin.Outcome {
	case outcomeSuccess:
		answered := findCandidate(cands, fin.AuthID)
		usageAuthID := fin.AuthID
		usageUID := candUID(answered, authUID)
		publishUsage(req.Model, upstreamModel, usageAuthID, started, collector.detail(), false, 0, "")
		recordUsageMap(req.Model, upstreamModel, usageAuthID, usageUID, started, collector.lastMap(), false, trace)
		onFailoverSuccess(req.AuthID, fin.AuthID, candRecordID(answered, fin.AuthID), authUID)
	case outcomeRateLimited, outcomeAccountExhausted:
		// Every eligible account was rejected before a chunk. Do not invoke
		// destructive lifecycle; the retry loop already quarantined accounts.
		usageAuthID, usageUID := failureIdentity(cands, fin, req.AuthID, authUID)
		publishUsage(req.Model, upstreamModel, usageAuthID, started, usage.Detail{}, true, fin.Status, fin.Body)
		recordUsageMap(req.Model, upstreamModel, usageAuthID, usageUID, started, collector.lastMap(), true, trace)
		streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", fin.Status, truncateRedacted(fin.Body, 200)))
	default: // outcomeStop
		if fin.Emitted {
			// Chunks were already streamed when the upstream failed — the
			// client has partial data. Record the attempt as failed (upstream
			// may have billed tokens seen in the usage chunks) and emit the
			// terminal error frame.
			answered := findCandidate(cands, fin.AuthID)
			usageAuthID := firstNonEmpty(fin.AuthID, req.AuthID)
			usageUID := candUID(answered, authUID)
			publishUsage(req.Model, upstreamModel, usageAuthID, started, collector.detail(), true, fin.Status, fin.Body)
			recordUsageMap(req.Model, upstreamModel, usageAuthID, usageUID, started, collector.lastMap(), true, trace)
			if fin.Status > 0 {
				streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", fin.Status, truncateRedacted(fin.Body, 200)))
			} else if fin.Err != nil {
				streamEmitError(streamID, fmt.Sprintf("upstream stream read error: %v", fin.Err))
			}
			return
		}
		usageAuthID, usageUID := failureIdentity(cands, fin, req.AuthID, authUID)
		publishUsage(req.Model, upstreamModel, usageAuthID, started, usage.Detail{}, true, fin.Status, fin.Body)
		recordUsageMap(req.Model, upstreamModel, usageAuthID, usageUID, started, collector.lastMap(), true, trace)
		if fin.Status > 0 && !isRateLimitResponse(fin.Status, fin.Body) && !isAccountExhaustedResponse(fin.Status, fin.Body) {
			go reconcileByUID(usageUID, fin.Status, fin.Body)
		}
		streamEmitError(streamID, failoverErrorMessage(fin.Status, fin.Body, fin.Err))
	}
}

// failoverErrorMessage formats the client-facing error for a failed stream:
// upstream HTTP errors keep their status prefix, plain errors (empty_stream,
// transport) surface their message.
func failoverErrorMessage(status int, body string, err error) string {
	if status > 0 {
		return fmt.Sprintf("upstream %d: %s", status, truncateRedacted(body, 200))
	}
	if err != nil {
		return err.Error()
	}
	return "workbuddy stream request failed"
}

// Test seam for the host stream sink; production retains the real callback.
var streamEmitSource = streamEmit

// pumpAttempt runs one upstream SSE pump against one account. Returns
// (emitted, statusCode, err): emitted reports whether any chunk reached the
// client, statusCode the upstream HTTP status (0 for transport failures).
// A 200 stream that closes without any data chunk is surfaced as
// errEmptyStream so the failover loop stops (never retried on another
// account) and the client gets a proper error instead of a silent success.
func pumpAttempt(httpReq *http.Request, streamID string, sseFramed bool, requestedModel, upstreamModel, authID, authUID string, started time.Time, collector *sseUsageCollector, traces ...*requestCacheTrace) (bool, int, error) {
	stream, statusCode, _, err := hostHTTPDoStream(httpReq)
	if trace := firstCacheTrace(traces); trace != nil {
		trace.finishAttempt(len(trace.Attempts)-1, statusCode)
	}
	if err != nil {
		return false, 0, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	if statusCode >= 400 {
		errPayload, _ := io.ReadAll(newHostStreamReader(stream))
		return false, statusCode, &upstreamStatusError{status: statusCode, body: string(errPayload)}
	}
	emitted := false
	err = scanCompletionSSE(newHostStreamReader(stream), func(content string) error {
		collector.feed(content)
		cleaned := cleanChunkJSON(content)
		if cleaned == "" {
			return nil
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		if err := streamEmitSource(streamID, []byte(cleaned)); err != nil {
			// Client disconnected / host closed stream — abort.
			return fmt.Errorf("stream_emit: %w", err)
		}
		emitted = true
		return nil
	})
	if err != nil {
		return emitted, 0, err
	}
	if !emitted {
		// The upstream closed without forwarding a single chunk to the client
		// (empty SSE, or every event was a no-op shell). Report it as an error
		// so the host does not treat the attempt as a silent success — which
		// would surface as a retryable empty_stream on the host side and bill
		// a second account for the same request.
		return emitted, 0, errEmptyStream
	}
	return emitted, 0, nil
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines. The host hands the
// plugin the inbound request path in Metadata, so we frame chunks ourselves for
// any entry path other than the native OpenAI chat-completions one.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// aggregateSSEWithCollector reads an upstream SSE stream and emits one chunk
// per data event. Empty tool-call shells are stripped and the trailing [DONE]
// is dropped (the host appends its own stream terminator). When sseFramed is
// true each payload is emitted as a "data: " line for cross-format
// translators; otherwise the payload is the raw JSON object and the host
// chat-completions writer adds the framing itself. A mid-stream read error
// aborts collection and is returned so the caller records the attempt as
// failed. The collector, when non-nil, observes raw upstream chunks for usage
// extraction.
func aggregateSSEWithCollector(r io.Reader, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, error) {
	var chunks []pluginapi.ExecutorStreamChunk
	err := scanCompletionSSE(r, func(content string) error {
		if collector != nil {
			collector.feed(content)
		}
		cleaned := cleanChunkJSON(content)
		if cleaned == "" {
			return nil
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(cleaned)})
		return nil
	})
	return chunks, err
}

// cleanChunkJSON strips only the known-problematic empty tool-call shells
// from choice deltas: a null/empty function_call and an empty tool_calls array
// (CodeBuddy emits these on the terminal chunk, and strict clients interpret
// them as a truncated tool call). Other empty-but-legal values are preserved:
// content:"" is a valid delta (pure tool-call chunk) and the role-only first
// chunk must survive so clients can establish the message role.
func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	changed := false
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			if v, present := delta["function_call"]; present && isEmptyValue(v) {
				delete(delta, "function_call")
				changed = true
			}
			if v, present := delta["tool_calls"]; present {
				if arr, isArr := v.([]any); isArr && len(arr) == 0 {
					delete(delta, "tool_calls")
					changed = true
				}
			}
			// Upstream often pads terminal/noop deltas with empty noise fields
			// that clients ignore but pollute wire size / some parsers.
			for _, noise := range []string{"extra_fields", "refusal", "reasoning_content"} {
				if v, present := delta[noise]; present && isEmptyValue(v) {
					delete(delta, noise)
					changed = true
				}
			}
			// Drop a fully-empty delta ONLY when the choice carries no other
			// signal (no finish_reason): e.g. {"delta":{"function_call":null}}
			// reduced to {}. A delta with role/content:"" is meaningful and
			// never reaches this branch (those fields are preserved above).
			if len(delta) == 0 {
				if fr, _ := choice["finish_reason"].(string); fr == "" {
					return ""
				}
			}
		}
	}
	if !changed {
		return s
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

// aggregateCompletion folds a full upstream SSE stream into one
// chat.completion object. Legacy wrapper kept for tests; production callers
// use aggregateCompletionCounted so empty streams can be distinguished.
func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	out, _, err := aggregateCompletionCounted(r, model)
	return out, err
}

// aggregateCompletionCounted folds a full upstream SSE stream into one
// chat.completion object and reports whether any data chunk was observed.
// sawData=false means the upstream closed before producing a single chunk
// (callers decide whether that is an error).
func aggregateCompletionCounted(r io.Reader, model string) ([]byte, bool, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	sawData := false
	// tool_calls arrive as streaming deltas: each chunk carries an index plus a
	// partial call (id/type/function.name on the first delta, argument text
	// fragments afterwards). Merge by index instead of appending raw fragments
	// so the folded completion holds whole calls.
	toolCalls := map[int]map[string]any{}
	var toolOrder []int
	scanErr := scanCompletionSSE(r, func(data string) error {
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			return fmt.Errorf("invalid upstream completion SSE JSON")
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) > 0 {
			sawData = true
		}
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						call, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						idx := 0
						if v, ok := call["index"].(float64); ok {
							idx = int(v)
						}
						merged, seen := toolCalls[idx]
						if !seen {
							merged = map[string]any{"index": idx}
							toolCalls[idx] = merged
							toolOrder = append(toolOrder, idx)
						}
						mergeToolCallDelta(merged, call)
					}
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
		return nil
	})
	// A mid-stream read failure means the folded completion is truncated. The
	// host discards the payload entirely when the plugin returns an error
	// (sdk/api/handlers executeWithPluginExecutor), so fail fast here instead
	// of assembling a partial completion nobody can safely consume.
	if scanErr != nil {
		return nil, sawData, scanErr
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		// Loop guard (0.9.7): a runaway upstream can fold thousands of
		// tool-call deltas into one completion. Refuse it loudly instead of
		// handing the host a truncated/looping payload.
		guarded, guardErr := guardToolCalls(calls)
		if guardErr != nil {
			return nil, sawData, guardErr
		}
		message["tool_calls"] = guarded
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-workbuddy"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": firstNonEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, sawData, err
	}
	return out, sawData, nil
}

// maxToolCallsPerCompletion bounds how many tool calls a single folded
// completion may carry. Anything above it is the runaway-loop signature seen
// in production (1.014 "echo skip" calls in one completion), never a real
// multi-tool turn.
const maxToolCallsPerCompletion = 32

// guardLogWriter receives loop-guard diagnostics. Tests swap it for a buffer;
// production writes to stderr, which the CPA host captures with the plugin
// logs. The plugin has no host log RPC, so stderr is the only channel.
var guardLogWriter io.Writer = os.Stderr

// guardLogf writes one guard diagnostic line.
func guardLogf(format string, args ...any) {
	if guardLogWriter == nil {
		return
	}
	fmt.Fprintf(guardLogWriter, "[workbuddy] "+format+"\n", args...)
}

// guardToolCalls validates the merged tool calls of one completion before they
// are emitted. Exact duplicates (same id, name and arguments) are folded into a
// single call, and the completion is capped at maxToolCallsPerCompletion.
//
// A completion that arrived with more call deltas than the cap is rejected with
// an error instead of being silently truncated: the host discards the payload
// when the plugin returns an error, so the client sees a clean failure rather
// than a partial tool loop. The cap is checked against the raw delta count, so
// an exact-duplicate storm cannot mask itself by folding down to a handful of
// calls.
func guardToolCalls(calls []map[string]any) ([]map[string]any, error) {
	total := len(calls)
	folded := foldDuplicateToolCalls(calls)
	if total > maxToolCallsPerCompletion || len(folded) > maxToolCallsPerCompletion {
		guardLogf("toolcall guard tripped: %d folded to %d", total, len(folded))
		return nil, fmt.Errorf("toolcall guard: %d tool calls in one completion (limit %d, %d after folding exact duplicates)", total, maxToolCallsPerCompletion, len(folded))
	}
	if len(folded) != total {
		guardLogf("toolcall guard tripped: %d folded to %d", total, len(folded))
	}
	return folded, nil
}

// foldDuplicateToolCalls drops repeated calls, keeping the first occurrence in
// order. Two calls are duplicates only when id, function.name and
// function.arguments all match AND the id is non-empty: a call without an id
// cannot be proven to be the same call, so it is always kept (the cap still
// bounds it). This keeps legitimately repeated calls (two identical reads,
// distinct ids) intact.
func foldDuplicateToolCalls(calls []map[string]any) []map[string]any {
	seen := make(map[string]struct{}, len(calls))
	folded := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		key, ok := toolCallIdentity(call)
		if !ok {
			folded = append(folded, call)
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		folded = append(folded, call)
	}
	return folded
}

// toolCallIdentity builds the duplicate key for one merged call. ok is false
// when the call carries no id, meaning it can never be treated as a duplicate.
func toolCallIdentity(call map[string]any) (string, bool) {
	id, _ := call["id"].(string)
	if strings.TrimSpace(id) == "" {
		return "", false
	}
	fn, _ := call["function"].(map[string]any)
	name, _ := fn["name"].(string)
	args, _ := fn["arguments"].(string)
	return id + "\x00" + name + "\x00" + args, true
}

// mergeToolCallDelta folds one streaming tool_call fragment into the merged
// call: scalar fields (id/type) are taken when first seen, function.name is
// concatenated (upstream may split it), and function.arguments text fragments
// are appended in arrival order.
func mergeToolCallDelta(merged, delta map[string]any) {
	for _, k := range []string{"id", "type"} {
		if _, present := merged[k]; !present {
			if v, ok := delta[k].(string); ok && v != "" {
				merged[k] = v
			}
		}
	}
	dfn, _ := delta["function"].(map[string]any)
	if dfn == nil {
		return
	}
	mfn, _ := merged["function"].(map[string]any)
	if mfn == nil {
		mfn = map[string]any{}
		merged["function"] = mfn
	}
	if v, ok := dfn["name"].(string); ok && v != "" {
		cur, _ := mfn["name"].(string)
		mfn["name"] = cur + v
	}
	if v, ok := dfn["arguments"].(string); ok && v != "" {
		cur, _ := mfn["arguments"].(string)
		mfn["arguments"] = cur + v
	}
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
