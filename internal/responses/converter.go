package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/local/codex-deepseek-proxy/internal/chat"
)

func ToChat(r Request) (chat.CompletionRequest, error) {
	if strings.TrimSpace(r.Model) == "" {
		return chat.CompletionRequest{}, errors.New("model is required")
	}
	if len(bytes.TrimSpace(r.Input)) == 0 || bytes.Equal(bytes.TrimSpace(r.Input), []byte("null")) {
		return chat.CompletionRequest{}, errors.New("input is required")
	}
	out := chat.CompletionRequest{
		Model: r.Model, Stream: true, Temperature: r.Temperature, MaxTokens: r.MaxOutputTokens,
		ParallelToolCalls: r.ParallelToolCalls,
	}
	if r.Instructions != "" {
		out.Messages = append(out.Messages, chat.Message{Role: "system", Content: r.Instructions})
	}
	msgs, err := convertInput(r.Input)
	if err != nil {
		return out, err
	}
	out.Messages = append(out.Messages, msgs...)
	for i, t := range r.Tools {
		switch t.Type {
		case "function":
			if t.Name == "" {
				return out, fmt.Errorf("tools[%d]: function name is required", i)
			}
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			if !json.Valid(params) {
				return out, fmt.Errorf("tools[%d].parameters is invalid JSON", i)
			}
			out.Tools = append(out.Tools, chat.Tool{Type: "function", Function: chat.ToolFunction{Name: t.Name, Description: t.Description, Parameters: params, Strict: t.Strict}})
		case "custom", "apply_patch":
			name := t.Name
			if name == "" && t.Type == "apply_patch" {
				name = "apply_patch"
			}
			if name == "" {
				return out, fmt.Errorf("tools[%d]: custom tool name is required", i)
			}
			// Chat-only providers do not understand Responses freeform tools. Wrap
			// their raw input in a one-field function schema; OutputState unwraps it
			// before returning a custom_tool_call to Codex.
			params := json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)
			out.Tools = append(out.Tools, chat.Tool{Type: "function", Function: chat.ToolFunction{Name: name, Description: customToolDescription(name, t.Description), Parameters: params}})
		default:
			// Hosted Responses tools such as web_search cannot be executed by an
			// OpenAI-compatible Chat Completions provider.
			continue
		}
	}
	out.ToolChoice = convertToolChoice(r.ToolChoice)
	return out, nil
}

func convertInput(raw json.RawMessage) ([]chat.Message, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []chat.Message{{Role: "user", Content: text}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("input must be a string or an array")
	}
	var msgs []chat.Message
	var pendingCalls []chat.ToolCall
	flushCalls := func() {
		if len(pendingCalls) > 0 {
			msgs = append(msgs, chat.Message{Role: "assistant", ToolCalls: pendingCalls})
			pendingCalls = nil
		}
	}
	for i, rawItem := range items {
		var item struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Input     string          `json:"input"`
			Output    json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return nil, fmt.Errorf("input[%d]: %w", i, err)
		}
		switch item.Type {
		case "function_call":
			if item.CallID == "" || item.Name == "" {
				return nil, fmt.Errorf("input[%d]: function_call needs call_id and name", i)
			}
			pendingCalls = append(pendingCalls, chat.ToolCall{ID: item.CallID, Type: "function", Function: chat.ToolCallFunction{Name: item.Name, Arguments: item.Arguments}})
		case "custom_tool_call":
			if item.CallID == "" || item.Name == "" {
				return nil, fmt.Errorf("input[%d]: custom_tool_call needs call_id and name", i)
			}
			args, err := json.Marshal(map[string]string{"value": item.Input})
			if err != nil {
				return nil, fmt.Errorf("input[%d].input: %w", i, err)
			}
			pendingCalls = append(pendingCalls, chat.ToolCall{ID: item.CallID, Type: "function", Function: chat.ToolCallFunction{Name: item.Name, Arguments: string(args)}})
		case "function_call_output", "custom_tool_call_output":
			flushCalls()
			if item.CallID == "" {
				return nil, fmt.Errorf("input[%d]: function_call_output needs call_id", i)
			}
			output, err := valueAsString(item.Output)
			if err != nil {
				return nil, fmt.Errorf("input[%d].output: %w", i, err)
			}
			msgs = append(msgs, chat.Message{Role: "tool", ToolCallID: item.CallID, Content: output})
		case "message", "":
			flushCalls()
			// Chat endpoints used by ArvanCloud accept system rather than the
			// Responses developer role. Keep its content and position intact.
			if item.Role == "developer" {
				item.Role = "system"
			}
			if item.Role != "user" && item.Role != "assistant" && item.Role != "system" {
				return nil, fmt.Errorf("input[%d]: unsupported role %q", i, item.Role)
			}
			content, err := contentText(item.Content)
			if err != nil {
				return nil, fmt.Errorf("input[%d].content: %w", i, err)
			}
			msgs = append(msgs, chat.Message{Role: item.Role, Content: content})
		default:
			return nil, fmt.Errorf("input[%d]: unsupported type %q", i, item.Type)
		}
	}
	flushCalls()
	return msgs, nil
}

func customToolDescription(name, description string) string {
	if name != "apply_patch" {
		return description
	}
	grammar := `Send one patch as raw text using this exact grammar. It must start with "*** Begin Patch" and end with "*** End Patch". Use "*** Add File: path", "*** Update File: path", or "*** Delete File: path" headers. Every content line in an Add File hunk must start with "+". Update hunks use "@@" context plus lines prefixed with " ", "+", or "-". Do not use standard unified-diff file headers. Example: *** Begin Patch\n*** Add File: example.txt\n+first line\n*** End Patch`
	if strings.TrimSpace(description) == "" {
		return grammar
	}
	return description + "\n\n" + grammar
}

func contentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("must be string or content array")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "input_text" && p.Type != "output_text" && p.Type != "text" {
			return "", fmt.Errorf("unsupported content type %q", p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

func valueAsString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	if !json.Valid(raw) {
		return "", errors.New("invalid JSON")
	}
	return string(raw), nil
}

func convertToolChoice(v any) any {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if m["type"] == "function" {
		if name, ok := m["name"].(string); ok {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	if m["type"] == "custom" {
		if name, ok := m["name"].(string); ok {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return v
}
