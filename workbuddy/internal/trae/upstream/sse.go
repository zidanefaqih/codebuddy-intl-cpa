package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// SSEError carries native business errors, including those returned with HTTP 200.
type SSEError struct {
	Code    int
	Message string
}

func (e *SSEError) Error() string { return fmt.Sprintf("Trae error %d: %s", e.Code, e.Message) }

type Call struct {
	Index    int          `json:"index"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type Completion struct {
	ID        string
	Model     string
	Created   int64
	Content   string
	Reasoning string
	Calls     []Call
	Usage     map[string]any
	Finish    string
}

func (c Completion) JSON() []byte {
	message := map[string]any{"role": "assistant", "content": c.Content}
	if c.Reasoning != "" {
		message["reasoning_content"] = c.Reasoning
	}
	if len(c.Calls) > 0 {
		calls := []map[string]any{}
		for _, tc := range c.Calls {
			calls = append(calls, map[string]any{"id": tc.ID, "type": "function", "function": tc.Function})
		}
		message["tool_calls"] = calls
	}
	out := map[string]any{"id": c.ID, "object": "chat.completion", "created": c.Created, "model": c.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": c.Finish}}}
	if c.Usage != nil {
		out["usage"] = c.Usage
	}
	b, _ := json.Marshal(out)
	return b
}

// Convert uses the same state machine for streaming and aggregation. emit receives
// JSON chunks without SSE framing; CPA owns final framing and the [DONE] marker.
func Convert(r io.Reader, model string, emit func([]byte) error) (Completion, error) {
	c := Completion{ID: fmt.Sprintf("chatcmpl-trae-%d", time.Now().UnixNano()), Model: model, Created: time.Now().Unix(), Finish: "stop"}
	calls := map[int]*Call{}
	ids := map[string]int{}
	seen := false
	done := false
	chunk := func(delta map[string]any, finish string) error {
		if emit == nil {
			return nil
		}
		if !seen && len(delta) > 0 {
			delta["role"] = "assistant"
			seen = true
		}
		choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		out := map[string]any{"id": c.ID, "object": "chat.completion.chunk", "created": c.Created, "model": model, "choices": []any{choice}}
		if finish != "" && c.Usage != nil {
			out["usage"] = c.Usage
		}
		b, _ := json.Marshal(out)
		return emit(b)
	}
	handle := func(event, data string) error {
		if data == "" {
			return nil
		}
		if data == "[DONE]" {
			done = true
			return nil
		}
		// Progress, metadata and other auxiliary events are not completion data.
		// Some contain JSON strings (e.g. "Processing_...") or plain text, so
		// select semantic events before decoding their object payloads.
		switch event {
		case "output", "token_usage", "error", "done":
		default:
			return nil
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			return fmt.Errorf("invalid Trae SSE JSON in %s", event)
		}
		str := func(key string) string { var v string; _ = json.Unmarshal(obj[key], &v); return v }
		switch event {
		case "error":
			var code int
			_ = json.Unmarshal(obj["code"], &code)
			message := str("message")
			if message == "" {
				message = str("error")
			}
			return &SSEError{code, message}
		case "token_usage":
			c.Usage = normalizeUsage([]byte(data))
		case "output":
			delta := map[string]any{}
			if text := str("response"); text != "" {
				c.Content += text
				delta["content"] = text
			}
			if text := str("reasoning_content"); text != "" {
				c.Reasoning += text
				delta["reasoning_content"] = text
			}
			var list []map[string]json.RawMessage
			raw := obj["tool_calls"]
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &list); err != nil {
					var one map[string]json.RawMessage
					if err = json.Unmarshal(raw, &one); err != nil {
						return fmt.Errorf("invalid native tool_calls")
					}
					list = []map[string]json.RawMessage{one}
				}
			}
			deltas := []map[string]any{}
			for pos, tc := range list {
				index := pos
				_ = json.Unmarshal(tc["index"], &index)
				if index < 0 {
					return fmt.Errorf("negative tool index")
				}
				var id string
				_ = json.Unmarshal(tc["id"], &id)
				if old, ok := ids[id]; id != "" && ok {
					index = old
				}
				fnRaw := tc["function_call"]
				if len(fnRaw) == 0 {
					fnRaw = tc["function"]
				}
				var fn map[string]json.RawMessage
				if json.Unmarshal(fnRaw, &fn) != nil {
					continue
				}
				var name string
				_ = json.Unmarshal(fn["name"], &name)
				var args string
				argRaw := fn["arguments"]
				if len(argRaw) > 0 && string(argRaw) != "null" {
					if json.Unmarshal(argRaw, &args) != nil {
						args = string(argRaw)
					}
				}
				call := calls[index]
				fresh := call == nil
				if fresh {
					if id == "" {
						id = fmt.Sprintf("call_trae_%d_%d", c.Created, index)
					}
					call = &Call{Index: index, ID: id, Type: "function"}
					calls[index] = call
					ids[id] = index
				}
				fnDelta := map[string]any{}
				if name != "" && call.Function.Name == "" {
					call.Function.Name = name
					fnDelta["name"] = name
				}
				if args != "" {
					call.Function.Arguments += args
					fnDelta["arguments"] = args
				}
				if len(fnDelta) > 0 {
					d := map[string]any{"index": index, "function": fnDelta}
					if fresh {
						d["id"] = call.ID
						d["type"] = "function"
					}
					deltas = append(deltas, d)
				}
			}
			if len(deltas) > 0 {
				delta["tool_calls"] = deltas
			}
			if len(delta) > 0 {
				if err := chunk(delta, ""); err != nil {
					return err
				}
			}
		case "done":
			done = true
			if reason := str("finish_reason"); reason != "" {
				c.Finish = reason
			}
		}
		return nil
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	event := ""
	data := []string{}
	dispatch := func() error { err := handle(event, strings.Join(data, "\n")); event = ""; data = nil; return err }
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return c, err
			}
			if done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return c, err
	}
	if !done && len(data) > 0 {
		if err := dispatch(); err != nil {
			return c, err
		}
	}
	if !done {
		return c, fmt.Errorf("Trae stream ended without a done event")
	}
	if c.Content == "" && c.Reasoning == "" && len(calls) == 0 {
		return c, fmt.Errorf("Trae returned an empty completion")
	}
	indices := []int{}
	for index := range calls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		call := calls[index]
		if call.Function.Name == "" {
			return c, fmt.Errorf("tool call has no function name")
		}
		c.Calls = append(c.Calls, *call)
	}
	if len(c.Calls) > 0 && c.Finish == "stop" {
		c.Finish = "tool_calls"
	}
	if err := chunk(map[string]any{}, c.Finish); err != nil {
		return c, err
	}
	return c, nil
}

// normalizeUsage maps Trae's raw token_usage payload onto the OpenAI usage
// shape. Unknown fields pass through untouched; known aliases are folded into
// the standard slots so clients and the panel see consistent data:
//   - cache_read_tokens / cached_tokens -> prompt_tokens_details.cached_tokens
//   - cache_write_tokens                -> kept as-is (OpenAI has no slot)
//   - reasoning_tokens                  -> completion_tokens_details.reasoning_tokens
//   - credits_consumed aliases          -> credits_consumed
func normalizeUsage(raw []byte) map[string]any {
	var usage map[string]any
	if err := json.Unmarshal(raw, &usage); err != nil || usage == nil {
		return nil
	}
	number := func(keys ...string) (float64, bool) {
		for _, key := range keys {
			v, ok := usage[key]
			if !ok {
				continue
			}
			n, ok := v.(float64)
			if !ok {
				continue
			}
			delete(usage, key)
			return n, true
		}
		return 0, false
	}
	details := func(slot string) map[string]any {
		if existing, ok := usage[slot].(map[string]any); ok {
			return existing
		}
		out := map[string]any{}
		usage[slot] = out
		return out
	}
	if cached, ok := number("cache_read_tokens", "cacheReadTokens", "cached_tokens", "cachedTokens", "cache_read_input_tokens"); ok {
		usage["cache_read_tokens"] = cached
		details("prompt_tokens_details")["cached_tokens"] = int64(cached)
	}
	if written, ok := number("cache_write_tokens", "cacheWriteTokens", "cache_creation_input_tokens"); ok {
		usage["cache_write_tokens"] = written
	}
	if reasoning, ok := number("reasoning_tokens", "reasoningTokens"); ok {
		details("completion_tokens_details")["reasoning_tokens"] = int64(reasoning)
	}
	if credits, ok := number("credits_consumed", "consumed_credits", "credit_cost", "credits_cost", "credits_float", "creditsFloat"); ok {
		usage["credits_consumed"] = credits
	}
	return usage
}
