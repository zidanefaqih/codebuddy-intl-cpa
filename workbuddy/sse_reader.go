package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// scanCompletionSSE reads SSE frames, forwarding only Chat Completions chunks
// and usage. Event names, comments, metadata and scalar progress notifications
// must never reach CPA's completion/Responses/Claude translators as chunks.
func scanCompletionSSE(r io.Reader, consume func(string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	event := ""
	var data []string
	frameBytes := 0
	done := false
	dispatch := func() error {
		name, payload := event, strings.Join(data, "\n")
		event, data = "", nil
		frameBytes = 0
		if payload == "[DONE]" || name == "done" {
			done = true
			return nil
		}
		if payload == "" {
			return nil
		}
		if name == "error" {
			return fmt.Errorf("upstream SSE error: %s", truncateRedacted(payload, 200))
		}
		switch name {
		case "", "message", "completion", "chunk", "output":
		default:
			return nil
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			// A valid scalar/array is an informational payload, not a chunk.
			if json.Valid([]byte(payload)) {
				return nil
			}
			return fmt.Errorf("invalid upstream completion SSE JSON")
		}
		if obj["error"] != nil {
			return fmt.Errorf("upstream SSE error: %s", truncateRedacted(payload, 200))
		}
		choices, _ := obj["choices"].([]any)
		usage, _ := obj["usage"].(map[string]any)
		if len(choices) == 0 && len(usage) == 0 {
			return nil
		}
		// Translators consume one data: line per chunk. SSE permits JSON split
		// across data fields; compact it before adding the outgoing framing.
		if strings.ContainsAny(payload, "\r\n") {
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(payload)); err != nil {
				return fmt.Errorf("invalid upstream completion SSE JSON")
			}
			payload = compact.String()
		}
		return consume(payload)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			if done {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			// Keep the former per-line bound for an entire multiline event.
			frameBytes += len(value) + 1
			if frameBytes > 4*1024*1024 {
				return fmt.Errorf("upstream SSE event exceeds 4 MiB")
			}
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("upstream stream read error: %w", err)
	}
	return dispatch()
}
