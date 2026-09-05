package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToChatStringAndInstructions(t *testing.T) {
	r := Request{Model: "deepseek-v4-pro", Instructions: "system", Input: json.RawMessage(`"hello"`)}
	c, err := ToChat(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Messages) != 2 || c.Messages[0].Role != "system" || c.Messages[1].Content != "hello" {
		t.Fatalf("unexpected messages: %#v", c.Messages)
	}
}

func TestToChatToolRoundTrip(t *testing.T) {
	strict := true
	parallel := false
	r := Request{Model: "deepseek-v4-pro", Input: json.RawMessage(`[
      {"type":"message","role":"user","content":[{"type":"input_text","text":"run tests"}]},
      {"type":"function_call","call_id":"call_123","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"},
      {"type":"function_call_output","call_id":"call_123","output":"ok"}
    ]`), Tools: []Tool{{Type: "function", Name: "exec_command", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`), Strict: &strict}}, ToolChoice: map[string]any{"type": "function", "name": "exec_command"}, ParallelToolCalls: &parallel}
	c, err := ToChat(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Messages) != 3 {
		t.Fatalf("got %d messages", len(c.Messages))
	}
	if c.Messages[1].ToolCalls[0].ID != "call_123" {
		t.Fatal("call_id was not preserved")
	}
	if c.Messages[2].Role != "tool" || c.Messages[2].ToolCallID != "call_123" || c.Messages[2].Content != "ok" {
		t.Fatalf("bad tool output: %#v", c.Messages[2])
	}
	if c.Tools[0].Function.Name != "exec_command" {
		t.Fatalf("bad tool conversion: %#v", c.Tools)
	}
	if c.Tools[0].Function.Strict == nil || !*c.Tools[0].Function.Strict {
		t.Fatalf("strict function schema was not preserved: %#v", c.Tools[0])
	}
	if c.ParallelToolCalls == nil || *c.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls was not preserved: %#v", c.ParallelToolCalls)
	}
	m := c.ToolChoice.(map[string]any)
	if m["type"] != "function" {
		t.Fatalf("bad choice: %#v", m)
	}
}

func TestToChatRejectsUnsupportedContent(t *testing.T) {
	_, err := ToChat(Request{Model: "x", Input: json.RawMessage(`[{"role":"user","content":[{"type":"input_image","image_url":"x"}]}]`)})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestToChatIgnoresResponsesNativeTools(t *testing.T) {
	r := Request{
		Model: "deepseek-v4-pro",
		Input: json.RawMessage(`"hello"`),
		Tools: []Tool{
			{Type: "function", Name: "shell", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Type: "web_search"},
		},
	}
	c, err := ToChat(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tools) != 1 || c.Tools[0].Function.Name != "shell" {
		t.Fatalf("function tools were not preserved: %#v", c.Tools)
	}
}

func TestToChatWrapsCustomToolsAndHistory(t *testing.T) {
	r := Request{
		Model: "deepseek-v4-pro",
		Input: json.RawMessage(`[
          {"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},
          {"type":"custom_tool_call_output","call_id":"call_patch","output":"Done!"}
        ]`),
		Tools:      []Tool{{Type: "custom", Name: "apply_patch", Description: "Apply a patch"}},
		ToolChoice: map[string]any{"type": "custom", "name": "apply_patch"},
	}
	c, err := ToChat(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tools) != 1 || c.Tools[0].Function.Name != "apply_patch" {
		t.Fatalf("custom tool was not wrapped: %#v", c.Tools)
	}
	if c.Tools[0].Function.Strict != nil {
		t.Fatalf("custom wrapper sent a strict flag that some Chat providers reject: %#v", c.Tools[0])
	}
	choice, _ := c.ToolChoice.(map[string]any)
	function, _ := choice["function"].(map[string]any)
	if choice["type"] != "function" || function["name"] != "apply_patch" {
		t.Fatalf("custom tool choice was not wrapped: %#v", c.ToolChoice)
	}
	if !strings.Contains(c.Tools[0].Function.Description, "*** Add File: path") || !strings.Contains(c.Tools[0].Function.Description, "+first line") {
		t.Fatalf("apply_patch grammar was not supplied to the chat model: %q", c.Tools[0].Function.Description)
	}
	var schema map[string]any
	if err = json.Unmarshal(c.Tools[0].Function.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["value"]; !ok {
		t.Fatalf("custom wrapper has no value property: %#v", schema)
	}
	if len(c.Messages) != 2 || len(c.Messages[0].ToolCalls) != 1 {
		t.Fatalf("custom history was not converted: %#v", c.Messages)
	}
	var args map[string]string
	if err = json.Unmarshal([]byte(c.Messages[0].ToolCalls[0].Function.Arguments), &args); err != nil || args["value"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom input was not wrapped: %q (%v)", c.Messages[0].ToolCalls[0].Function.Arguments, err)
	}
	if c.Messages[1].Role != "tool" || c.Messages[1].ToolCallID != "call_patch" || c.Messages[1].Content != "Done!" {
		t.Fatalf("custom output was not converted: %#v", c.Messages[1])
	}
}
