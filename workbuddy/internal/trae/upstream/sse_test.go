package upstream

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestConvertTextReasoningUsage(t *testing.T) {
	src := ": keepalive\r\n\r\nevent: output\r\ndata: {\"reasoning_content\":\"想\",\"response\":\"你\"}\r\n\r\nevent: output\ndata: {\"response\":\"好\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}\n\nevent: done\ndata: {}\n\n"
	var chunks []map[string]any
	c, err := Convert(strings.NewReader(src), "chosen-model", func(b []byte) error {
		var v map[string]any
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		chunks = append(chunks, v)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Content != "你好" || c.Reasoning != "想" || c.Model != "chosen-model" || c.Finish != "stop" || c.Usage["total_tokens"] != float64(15) {
		t.Fatalf("unexpected completion: %+v", c)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks=%d", len(chunks))
	}
	for _, chunk := range chunks {
		if chunk["model"] != "chosen-model" {
			t.Fatal("lost model")
		}
	}
	last := chunks[2]["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "stop" {
		t.Fatal("no terminal stop")
	}
	aggregated, err := Convert(strings.NewReader(src), "chosen-model", nil)
	if err != nil || aggregated.Content != c.Content || aggregated.Reasoning != c.Reasoning {
		t.Fatal("stream and aggregation differ", err)
	}
}

func TestConvertFragmentedTools(t *testing.T) {
	src := "event: output\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"tool-1\",\"function_call\":{\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\"}}]}\n\nevent: output\ndata: {\"tool_calls\":[{\"index\":0,\"function_call\":{\"arguments\":\"\\\"上海\\\"}\"}},{\"index\":1,\"id\":\"tool-2\",\"function_call\":{\"name\":\"clock\",\"arguments\":{}}}]}\n\nevent: done\ndata: {}\n\n"
	var chunks [][]byte
	c, err := Convert(strings.NewReader(src), "tools-model", func(b []byte) error { chunks = append(chunks, b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if c.Finish != "tool_calls" || len(c.Calls) != 2 || c.Calls[0].Function.Arguments != `{"city":"上海"}` || c.Calls[1].Function.Arguments != `{}` {
		t.Fatalf("bad tool reconstruction: %+v", c)
	}
	var obj map[string]any
	_ = json.Unmarshal(c.JSON(), &obj)
	calls := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
	if _, found := calls[0].(map[string]any)["index"]; found {
		t.Fatal("stream index leaked into final tool call")
	}
	if !strings.Contains(string(chunks[len(chunks)-1]), `"finish_reason":"tool_calls"`) {
		t.Fatal("missing tools finish reason")
	}
}

func TestConvertErrorsAndPrematureEOF(t *testing.T) {
	for name, src := range map[string]string{"truncated": "event: output\ndata: {\"response\":\"partial\"}\n\n", "empty": "event: done\ndata: {}\n\n", "malformed": "event: output\ndata: nope\n\n", "tool-no-name": "event: output\ndata: {\"tool_calls\":[{\"function_call\":{\"arguments\":\"{}\"}}]}\n\nevent: done\ndata: {}\n\n"} {
		t.Run(name, func(t *testing.T) {
			if _, err := Convert(strings.NewReader(src), "m", nil); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	_, err := Convert(strings.NewReader("event: error\ndata: {\"code\":1005,\"message\":\"plan limit\"}\n\n"), "m", nil)
	var business *SSEError
	if !errors.As(err, &business) || business.Code != 1005 {
		t.Fatal("lost business error", err)
	}
	sentinel := errors.New("downstream closed")
	_, err = Convert(strings.NewReader("event: output\ndata: {\"response\":\"x\"}\n\nevent: done\ndata: {}\n\n"), "m", func([]byte) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatal("lost downstream cancellation", err)
	}
}

func TestConvertAuxiliaryEvents(t *testing.T) {
	// Trae can send scalar progress notices before the first output. Auxiliary
	// events may also change shape independently of the completion protocol.
	src := "event:progress_notice\ndata:\"Processing_1791709582\"\n\n" +
		"event:metadata\ndata:null\n\nevent:future_notice\ndata:processing\n\n" +
		"event:output\ndata:{\"response\":\"answer\"}\n\n" +
		"event:progress_notice\ndata:[\"still processing\"]\n\n" +
		"event:token_usage\ndata:{\"total_tokens\":6}\n\nevent:done\ndata:{}\n\n"
	for _, stream := range []bool{false, true} {
		var chunks int
		var emit func([]byte) error
		if stream {
			emit = func([]byte) error { chunks++; return nil }
		}
		c, err := Convert(strings.NewReader(src), "Doubao-Seed-2.0-Code", emit)
		if err != nil || c.Content != "answer" || c.Usage["total_tokens"] != float64(6) {
			t.Fatalf("stream=%v completion=%+v error=%v", stream, c, err)
		}
		if stream && chunks != 2 {
			t.Fatalf("auxiliary event emitted a completion chunk: %d", chunks)
		}
	}
	// Ignoring a progress event must not turn a truncated stream into success.
	if _, err := Convert(strings.NewReader("event:progress_notice\ndata:\"Processing\"\n\n"), "m", nil); err == nil {
		t.Fatal("accepted a progress-only stream")
	}
}

func TestPrepareFullToolHistory(t *testing.T) {
	src := []byte(`{"model":"exact","messages":[{"role":"system","content":"system"},{"role":"user","content":"weather"},{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"weather","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"晴"},{"role":"user","content":"continue"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"weather"}}}`)
	var obj map[string]any
	if err := json.Unmarshal(PrepareBody(src), &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 5 || obj["model"] != "exact" || obj["config_name"] != "exact" || obj["function"] != Function || obj["stream"] != true || obj["tool_choice"] != "weather" {
		t.Fatal("lost request semantics", obj)
	}
	tool := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tool["function_call"] == nil || tool["function"] != nil || msgs[3].(map[string]any)["tool_call_id"] != "a" {
		t.Fatal("tool history changed")
	}
	params := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"]
	if params != `{"type":"object"}` {
		t.Fatal("parameters must be encoded string", params)
	}
}

func TestNormalizeUsageCacheFields(t *testing.T) {
	src := "event: output\ndata: {\"response\":\"x\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120,\"cache_read_tokens\":64,\"cache_write_tokens\":12,\"reasoning_tokens\":8,\"creditsFloat\":1.5}\n\nevent: done\ndata: {}\n\n"
	c, err := Convert(strings.NewReader(src), "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	u := c.Usage
	if u == nil {
		t.Fatal("usage dropped")
	}
	prompt, ok := u["prompt_tokens_details"].(map[string]any)
	if !ok || prompt["cached_tokens"] != int64(64) {
		t.Fatalf("cached_tokens not mapped: %+v", u)
	}
	completion, ok := u["completion_tokens_details"].(map[string]any)
	if !ok || completion["reasoning_tokens"] != int64(8) {
		t.Fatalf("reasoning_tokens not mapped: %+v", u)
	}
	if u["cache_read_tokens"] != float64(64) || u["cache_write_tokens"] != float64(12) {
		t.Fatalf("cache counters lost: %+v", u)
	}
	if u["credits_consumed"] != 1.5 {
		t.Fatalf("credits alias lost: %+v", u)
	}
	if _, leaked := u["cacheReadTokens"]; leaked {
		t.Fatal("alias key leaked")
	}
	if _, leaked := u["creditsFloat"]; leaked {
		t.Fatal("credits alias leaked")
	}
	// Alternate aliases converge to the same slots.
	raw := normalizeUsage([]byte(`{"cacheReadTokens":9,"reasoningTokens":3,"consumed_credits":2}`))
	if raw["cache_read_tokens"] != float64(9) || raw["credits_consumed"] != float64(2) || raw["cacheReadTokens"] != nil {
		t.Fatalf("aliases not normalized: %+v", raw)
	}
	if normalizeUsage([]byte(`not-json`)) != nil {
		t.Fatal("malformed usage must be dropped")
	}
}
