package responses

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/local/codex-deepseek-proxy/internal/chat"
)

func TestTextStreamLifecycle(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := NewSSEWriter(rec)
	s := NewOutputState("deepseek-v4-pro", w)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	a, b := "Hel", "lo"
	finish := "stop"
	_ = s.ApplyChunk(chat.CompletionChunk{Choices: []chat.Choice{{Delta: chat.Delta{Content: &a}}}})
	_ = s.ApplyChunk(chat.CompletionChunk{Choices: []chat.Choice{{Delta: chat.Delta{Content: &b}, FinishReason: &finish}}})
	if _, err := s.Complete(); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, event := range []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"} {
		if !strings.Contains(body, "event: "+event) {
			t.Errorf("missing %s", event)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatal("leaked Chat [DONE]")
	}
	for i := int64(1); i <= 9; i++ {
		if !strings.Contains(body, `"sequence_number":`+itoa(i)) {
			t.Fatalf("missing sequence %d in %s", i, body)
		}
	}
}

func TestToolCallAccumulationAndCallID(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := NewSSEWriter(rec)
	s := NewOutputState("deepseek-v4-pro", w)
	_ = s.Start()
	first := chat.ToolCallDelta{
		Index: 0,
		ID:    "call_123",
		Function: chat.ToolCallFunctionDelta{
			Name:      "exec_command",
			Arguments: `{"cmd":"go `,
		},
	}
	_ = s.ApplyChunk(chat.CompletionChunk{Choices: []chat.Choice{{Delta: chat.Delta{ToolCalls: []chat.ToolCallDelta{first}}}}})
	finish := "tool_calls"
	second := chat.ToolCallDelta{Index: 0, Function: chat.ToolCallFunctionDelta{Arguments: `test ./..."}`}}
	_ = s.ApplyChunk(chat.CompletionChunk{Choices: []chat.Choice{{Delta: chat.Delta{ToolCalls: []chat.ToolCallDelta{second}}, FinishReason: &finish}}})
	resp, err := s.Complete()
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.function_call_arguments.delta") || !strings.Contains(body, "response.function_call_arguments.done") {
		t.Fatalf("missing tool events: %s", body)
	}
	if !strings.Contains(body, `"call_id":"call_123"`) {
		t.Fatal("call ID changed")
	}
	out := resp["output"].([]any)
	item := out[0].(map[string]any)
	if item["arguments"] != `{"cmd":"go test ./..."}` {
		t.Fatalf("bad arguments: %q", item["arguments"])
	}
}

func TestReasoningIsHidden(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := NewSSEWriter(rec)
	s := NewOutputState("m", w)
	_ = s.Start()
	reason := "secret chain"
	_ = s.ApplyChunk(chat.CompletionChunk{Choices: []chat.Choice{{Delta: chat.Delta{ReasoningContent: &reason}}}})
	_, _ = s.Complete()
	if strings.Contains(rec.Body.String(), reason) {
		t.Fatal("reasoning leaked into response")
	}
}

func TestMultipleParallelToolCalls(t *testing.T) {
	s := NewOutputState("m", nil)
	finish := "tool_calls"
	err := s.ApplyChunk(chat.CompletionChunk{Choices: []chat.Choice{{
		Delta: chat.Delta{ToolCalls: []chat.ToolCallDelta{
			{Index: 0, ID: "call_a", Function: chat.ToolCallFunctionDelta{Name: "first", Arguments: `{}`}},
			{Index: 1, ID: "call_b", Function: chat.ToolCallFunctionDelta{Name: "second", Arguments: `{"x":1}`}},
		}},
		FinishReason: &finish,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.Complete()
	if err != nil {
		t.Fatal(err)
	}
	out := resp["output"].([]any)
	if len(out) != 2 || out[0].(map[string]any)["call_id"] != "call_a" || out[1].(map[string]any)["call_id"] != "call_b" {
		t.Fatalf("parallel calls lost or reordered: %#v", out)
	}
}

func TestResponsePreservesToolConfiguration(t *testing.T) {
	parallel := false
	strict := true
	s := NewOutputState("m", nil)
	s.ConfigureRequest(Request{
		ToolChoice:        "required",
		ParallelToolCalls: &parallel,
		Tools:             []Tool{{Type: "function", Name: "ping", Strict: &strict}},
	})
	resp, err := s.Complete()
	if err != nil {
		t.Fatal(err)
	}
	if resp["tool_choice"] != "required" || resp["parallel_tool_calls"] != false {
		t.Fatalf("tool configuration was not preserved: %#v", resp)
	}
	tools, ok := resp["tools"].([]Tool)
	if !ok || len(tools) != 1 || tools[0].Name != "ping" || tools[0].Strict == nil || !*tools[0].Strict {
		t.Fatalf("function definitions were not preserved: %#v", resp["tools"])
	}
}

func TestCustomToolCallIsUnwrappedForCodex(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := NewSSEWriter(rec)
	s := NewOutputState("m", w)
	s.ConfigureRequest(Request{Tools: []Tool{{Type: "custom", Name: "apply_patch"}}})
	_ = s.Start()
	finish := "tool_calls"
	err := s.ApplyChunk(chat.CompletionChunk{Choices: []chat.Choice{{
		Delta: chat.Delta{ToolCalls: []chat.ToolCallDelta{{
			Index: 0,
			ID:    "call_patch",
			Function: chat.ToolCallFunctionDelta{
				Name:      "apply_patch",
				Arguments: `{"input":"*** Begin Patch\n*** End Patch"}`,
			},
		}}},
		FinishReason: &finish,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.Complete()
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, event := range []string{"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", "response.output_item.done"} {
		if !strings.Contains(body, "event: "+event) {
			t.Fatalf("missing %s in %s", event, body)
		}
	}
	if strings.Contains(body, "response.function_call_arguments") {
		t.Fatalf("custom call leaked function argument events: %s", body)
	}
	item := resp["output"].([]any)[0].(map[string]any)
	if item["type"] != "custom_tool_call" || item["name"] != "apply_patch" || item["input"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("bad custom output item: %#v", item)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
