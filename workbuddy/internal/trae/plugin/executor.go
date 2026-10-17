package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/zidanefaqih/codebuddy-intl-cpa/workbuddy/internal/trae/upstream"
)

type executeRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
	HTTPClientID   string `json:"http_client_id,omitempty"`
}
type candidate struct {
	id, name string
	raw      []byte
}

func (e *Engine) candidates(req executeRequest) []candidate {
	out := []candidate{}
	first, _ := parseCredential(req.StorageJSON)
	if !e.cooling(req.AuthID) && (first == nil || !e.cooling(first.UID)) && !disabled(req.StorageJSON) && !modelDisabled(req.StorageJSON, req.Model) {
		out = append(out, candidate{req.AuthID, "", req.StorageJSON})
	}
	files, err := e.listAuths()
	if err != nil {
		return out
	}
	var routing struct {
		Prefix   string `json:"prefix"`
		ProxyURL string `json:"proxy_url"`
		Priority int    `json:"priority"`
	}
	_ = json.Unmarshal(req.StorageJSON, &routing)
	for _, f := range files {
		if len(out) >= e.settings().MaxAttempts {
			break
		}
		if unavailable(f) || f.AuthIndex == req.AuthID || f.ID == req.AuthID || f.Name == req.AuthID || e.cooling(f.AuthIndex) {
			continue
		}
		phys, err := e.getAuth(f.AuthIndex)
		if err != nil || disabled(phys.JSON) || modelDisabled(phys.JSON, req.Model) {
			continue
		}
		a, err := parseCredential(phys.JSON)
		if err != nil || e.cooling(a.UID) || first != nil && first.UID == a.UID {
			continue
		}
		// Host selection already applied routing policies. Only use an equivalent
		// unprefixed account for early failover; never cross a credential namespace.
		var other struct {
			Prefix   string   `json:"prefix"`
			ProxyURL string   `json:"proxy_url"`
			Priority int      `json:"priority"`
			Excluded []string `json:"excluded-models"`
		}
		_ = json.Unmarshal(phys.JSON, &other)
		if routing.Prefix != "" || other.Prefix != "" || other.ProxyURL != routing.ProxyURL || other.Priority != routing.Priority || len(other.Excluded) > 0 {
			continue
		}
		modelRaw, modelErr := e.models(JSON(pluginapi.AuthModelRequest{AuthID: f.AuthIndex, StorageJSON: phys.JSON}))
		var modelEnv struct{ Result pluginapi.ModelResponse }
		_ = json.Unmarshal(modelRaw, &modelEnv)
		supported := false
		for _, m := range modelEnv.Result.Models {
			if m.ID == req.Model {
				supported = true
				break
			}
		}
		if modelErr != nil || !supported {
			continue
		}
		out = append(out, candidate{f.AuthIndex, phys.Name, phys.JSON})
	}
	return out
}
func (e *Engine) cooling(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return time.Now().Before(e.cooldown[id])
}
func (e *Engine) cooldownFor(id string, d time.Duration) {
	e.mu.Lock()
	e.cooldown[id] = time.Now().Add(d)
	e.mu.Unlock()
}
func (e *Engine) pick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	if req.Provider != Provider {
		return OK(pluginapi.SchedulerPickResponse{}), nil
	}
	anyCooling := false
	for _, c := range req.Candidates {
		if e.cooling(c.ID) || e.cooling(c.Attributes["uid"]) {
			anyCooling = true
			break
		}
	}
	if !anyCooling {
		return OK(pluginapi.SchedulerPickResponse{}), nil
	}
	for _, c := range req.Candidates {
		if c.Provider != Provider || strings.EqualFold(c.Status, "disabled") || strings.EqualFold(c.Status, "error") || e.cooling(c.ID) || e.cooling(c.Attributes["uid"]) {
			continue
		}
		if value, ok := c.Metadata["disabled"].(bool); ok && value {
			continue
		}
		return OK(pluginapi.SchedulerPickResponse{Handled: true, AuthID: c.ID}), nil
	}
	return OK(pluginapi.SchedulerPickResponse{}), nil
}
func (e *Engine) execute(raw []byte, stream bool) ([]byte, error) {
	var req executeRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	if req.AuthProvider != "" && req.AuthProvider != Provider {
		return nil, &WireError{"invalid_auth", "wrong provider for Trae executor", 400}
	}
	// CPA normalizes the capability alias "chat-completions" to "openai"
	// before building ExecutorRequest. SourceFormat describes the translated
	// payload here; Format describes the plugin's output protocol.
	if req.Format != "" && !chatCompletionsFormat(req.Format) {
		return nil, &WireError{"unsupported_format", fmt.Sprintf("Trae executor requires CPA-normalized OpenAI Chat Completions (Format=%q, SourceFormat=%q)", req.Format, req.SourceFormat), 400}
	}
	src := req.Payload
	if len(src) == 0 {
		src = req.OriginalRequest
	}
	if len(src) > 8<<20 {
		return nil, &WireError{"request_too_large", "chat request exceeds 8 MiB", 413}
	}
	var body map[string]any
	if json.Unmarshal(src, &body) != nil || body == nil {
		return nil, &WireError{"invalid_request", "invalid chat request", 400}
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, &WireError{"invalid_request", "messages must be a nonempty array", 400}
	}
	restoreDeveloperRoles(body, req.OriginalRequest)
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model, _ = body["model"].(string)
	}
	if model == "" {
		return nil, &WireError{"invalid_request", "model is required", 400}
	}
	if strings.EqualFold(model, "work") || strings.HasSuffix(strings.ToLower(model), "-official") {
		return nil, &WireError{"unsupported_model", "Work models are not supported by this SOLO plugin", 400}
	}
	body["model"] = model
	req.Model = model
	prepared := upstream.PrepareBody(JSON(body))
	cands := e.candidates(req)
	if len(cands) == 0 {
		return nil, &WireError{"no_healthy_account", "no enabled Trae account available", 503}
	}
	if stream && req.StreamID != "" {
		go func() {
			result, err := e.run(req, cands, prepared, model, true, func(chunk []byte) error {
				path, _ := req.Metadata["request_path"].(string)
				if path != "/v1/chat/completions" && path != "/v1/completions" {
					chunk = append(append([]byte("data: "), chunk...), []byte("\n\n")...)
				}
				return e.rpc(pluginabi.MethodHostStreamEmit, map[string]any{"stream_id": req.StreamID, "payload": chunk}, nil)
			})
			_ = result
			if err != nil {
				_ = e.rpc(pluginabi.MethodHostStreamEmit, map[string]string{"stream_id": req.StreamID, "error": redact(err.Error())}, nil)
			}
			_ = e.rpc(pluginabi.MethodHostStreamClose, map[string]string{"stream_id": req.StreamID}, nil)
		}()
		return OK(map[string]any{"headers": streamHeaders()}), nil
	}
	chunks := []pluginapi.ExecutorStreamChunk{}
	var emit func([]byte) error
	if stream {
		emit = func(chunk []byte) error {
			path, _ := req.Metadata["request_path"].(string)
			if path != "/v1/chat/completions" && path != "/v1/completions" {
				chunk = append(append([]byte("data: "), chunk...), []byte("\n\n")...)
			}
			chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: chunk})
			return nil
		}
	}
	result, err := e.run(req, cands, prepared, model, stream, emit)
	if err != nil {
		return nil, err
	}
	if stream {
		return OK(map[string]any{"headers": streamHeaders(), "chunks": chunks}), nil
	}
	return OK(pluginapi.ExecutorResponse{Payload: result.JSON(), Headers: http.Header{"Content-Type": []string{"application/json"}}}), nil
}

func chatCompletionsFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "none", "openai", "chat-completions", "chat_completions", "openai-chat-completions", "openai_chat_completions":
		return true
	default:
		return false
	}
}
func streamHeaders() http.Header {
	return http.Header{"Content-Type": []string{"text/event-stream"}, "Cache-Control": []string{"no-cache"}, "X-Accel-Buffering": []string{"no"}}
}
func (e *Engine) run(req executeRequest, cands []candidate, body []byte, model string, stream bool, emit func([]byte) error) (upstream.Completion, error) {
	start := time.Now()
	var last error
	var completion upstream.Completion
	lastAccount := ""
	attempts := 0
	defer func() {
		e.record(Usage{Time: start.UTC().Format(time.RFC3339), Model: model, Account: lastAccount, Stream: stream, Success: last == nil, Error: errorText(last), DurationMS: time.Since(start).Milliseconds(), Attempts: attempts, Tokens: completion.Usage})
	}()
	limit := e.settings().MaxAttempts
	for _, cand := range cands {
		if attempts >= limit {
			break
		}
		attempts++
		a, storage, err := e.ensureAuth(cand.id, cand.raw, false, proxyURL(cand.raw), req.HostCallbackID)
		if err != nil {
			last = err
			continue
		}
		lastAccount = a.UID
		if modelDisabled(storage, model) {
			last = &WireError{"model_disabled", "model is disabled for this Trae account", 403}
			continue
		}
		cool := func(d time.Duration) { e.cooldownFor(cand.id, d); e.cooldownFor(a.UID, d) }
		client := e.clientFactory(proxyURL(storage), req.HostCallbackID)
		httpReq, err := http.NewRequestWithContext(e.ctx, http.MethodPost, client.AgentHost+upstream.EpChat, bytes.NewReader(body))
		if err != nil {
			last = err
			break
		}
		upstream.SOLOHeaders(httpReq, a, true)
		resp, err := client.StreamHTTP.Do(httpReq)
		if err != nil {
			last = err
			break
		}
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			last = &WireError{"upstream_error", fmt.Sprintf("Trae HTTP %d: %s", resp.StatusCode, redact(string(raw))), resp.StatusCode}
			kind := upstream.Classify(resp.StatusCode, string(raw))
			switch kind {
			case upstream.ErrSoftRate:
				cool(time.Minute)
				continue
			case upstream.ErrPlanLimit:
				cool(12 * time.Hour)
				continue
			case upstream.ErrSessionDead:
				cool(time.Hour)
				continue
			}
			break
		}
		emitted := false
		wrapped := func(chunk []byte) error {
			if emit != nil {
				if err := emit(chunk); err != nil {
					return err
				}
				emitted = true
			}
			return nil
		}
		completion, err = upstream.Convert(resp.Body, model, wrapped)
		_ = resp.Body.Close()
		if err == nil {
			last = nil
			return completion, nil
		}
		last = err
		var se *upstream.SSEError
		if !emitted && errors.As(err, &se) {
			switch se.Code {
			case 4011:
				cool(time.Minute)
				continue
			case 1005, 4008:
				cool(12 * time.Hour)
				continue
			case 1001:
				cool(time.Hour)
				continue
			}
		}
		break
	}
	if last == nil {
		last = &WireError{"no_healthy_account", "all Trae accounts are unavailable", 503}
	}
	return completion, last
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return redact(err.Error())
}

// Check again after reading the current physical credential: the host registry
// and an in-flight request may predate a settings update.
func modelDisabled(raw []byte, model string) bool {
	var settings struct {
		Disabled     []string `json:"disabled_models"`
		Excluded     []string `json:"excluded_models"`
		HostExcluded []string `json:"excluded-models"`
	}
	_ = json.Unmarshal(raw, &settings)
	for _, pattern := range append(append(settings.Disabled, settings.Excluded...), settings.HostExcluded...) {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		for _, id := range []string{strings.ToLower(model), "trae/" + strings.ToLower(model)} {
			if matched, _ := path.Match(pattern, id); matched {
				return true
			}
		}
	}
	return false
}
