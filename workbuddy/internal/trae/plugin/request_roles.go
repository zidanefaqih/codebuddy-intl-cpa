package plugin

import (
	"encoding/json"
	"reflect"
)

// CPA 8.0.23 translates Responses developer messages to user messages. Recover
// their role from the raw client request while retaining CPA's tool/history
// conversion. Match text and order rather than guessing roles from positions.
func restoreDeveloperRoles(body map[string]any, original []byte) {
	var raw map[string]any
	if json.Unmarshal(original, &raw) != nil {
		return
	}
	input, ok := raw["input"].([]any)
	if !ok {
		return
	}
	messages, _ := body["messages"].([]any)
	cursor := 0
	if instructions, ok := raw["instructions"].(string); ok && len(messages) > 0 {
		m, _ := messages[0].(map[string]any)
		if m["role"] == "system" && reflect.DeepEqual(messageText(m["content"]), []string{instructions}) {
			cursor = 1
		}
	}
	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok || (m["type"] != nil && m["type"] != "message" && m["type"] != "") {
			continue
		}
		role, _ := m["role"].(string)
		if role != "developer" && role != "system" && role != "user" && role != "assistant" {
			continue
		}
		text := messageText(m["content"])
		if len(text) == 0 {
			continue
		}
		for i := cursor; i < len(messages); i++ {
			translated, ok := messages[i].(map[string]any)
			if !ok || !reflect.DeepEqual(text, messageText(translated["content"])) {
				continue
			}
			translatedRole, _ := translated["role"].(string)
			if translatedRole != role && !(role == "developer" && (translatedRole == "user" || translatedRole == "system")) {
				continue
			}
			if role == "developer" {
				translated["role"] = "system"
			}
			cursor = i + 1
			break
		}
	}
}

// Only compare wholly textual messages; matching just the text in a mixed
// image/file message could elevate a different user turn to an instruction.
func messageText(content any) []string {
	if s, ok := content.(string); ok {
		return []string{s}
	}
	parts, ok := content.([]any)
	if !ok {
		return nil
	}
	text := make([]string, 0, len(parts))
	for _, part := range parts {
		p, ok := part.(map[string]any)
		if !ok {
			return nil
		}
		switch p["type"] {
		case nil, "", "text", "input_text", "output_text":
		default:
			return nil
		}
		s, ok := p["text"].(string)
		if !ok {
			return nil
		}
		text = append(text, s)
	}
	return text
}
