package responses

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/local/codex-deepseek-proxy/internal/chat"
	"github.com/local/codex-deepseek-proxy/internal/ids"
)

type OutputState struct {
	ResponseID        string
	Model             string
	CreatedAt         int64
	Writer            *SSEWriter
	Started           bool
	FinishReason      string
	Usage             *chat.Usage
	ToolChoice        any
	ParallelToolCalls bool
	Tools             []Tool

	nextOutput int
	text       *textState
	tools      map[int]*toolState
	toolKinds  map[string]string
}

type textState struct {
	ID          string
	OutputIndex int
	Text        strings.Builder
}

type toolState struct {
	ItemID      string
	CallID      string
	Name        strings.Builder
	Arguments   strings.Builder
	OutputIndex int
	Added       bool
	EmittedArgs int
	Kind        string
}

func NewOutputState(model string, writer *SSEWriter) *OutputState {
	return &OutputState{ResponseID: ids.New("resp"), Model: model, CreatedAt: time.Now().Unix(), Writer: writer, ToolChoice: "auto", ParallelToolCalls: true, tools: make(map[int]*toolState), toolKinds: make(map[string]string)}
}

func (s *OutputState) ConfigureRequest(r Request) {
	if r.ToolChoice != nil {
		s.ToolChoice = r.ToolChoice
	}
	if r.ParallelToolCalls != nil {
		s.ParallelToolCalls = *r.ParallelToolCalls
	}
	s.Tools = append(s.Tools[:0], r.Tools...)
	clear(s.toolKinds)
	for _, tool := range r.Tools {
		name, kind := tool.Name, tool.Type
		if kind == "apply_patch" && name == "" {
			name, kind = "apply_patch", "custom"
		}
		if kind == "custom" || kind == "function" {
			s.toolKinds[name] = kind
		}
	}
}

func (s *OutputState) Start() error {
	if s.Started {
		return nil
	}
	s.Started = true
	if s.Writer == nil {
		return nil
	}
	if err := s.Writer.Send("response.created", map[string]any{"response": s.response("in_progress", false)}); err != nil {
		return err
	}
	return s.Writer.Send("response.in_progress", map[string]any{"response": s.response("in_progress", false)})
}

func (s *OutputState) ApplyChunk(c chat.CompletionChunk) error {
	if c.Usage != nil {
		s.Usage = c.Usage
	}
	for _, choice := range c.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			if err := s.addText(*choice.Delta.Content); err != nil {
				return err
			}
		}
		// reasoning_content is intentionally consumed but not exposed.
		for _, tc := range choice.Delta.ToolCalls {
			if err := s.addToolDelta(tc); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil {
			s.FinishReason = *choice.FinishReason
		}
	}
	return nil
}

func (s *OutputState) ApplyCompletion(c chat.Completion) error {
	if c.Usage != nil {
		s.Usage = c.Usage
	}
	for _, choice := range c.Choices {
		content, _ := choice.Message.Content.(string)
		if content != "" {
			if err := s.addText(content); err != nil {
				return err
			}
		}
		for i, tc := range choice.Message.ToolCalls {
			d := chat.ToolCallDelta{Index: i, ID: tc.ID, Type: tc.Type, Function: chat.ToolCallFunctionDelta{Name: tc.Function.Name, Arguments: tc.Function.Arguments}}
			if err := s.addToolDelta(d); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil {
			s.FinishReason = *choice.FinishReason
		}
	}
	return nil
}

func (s *OutputState) addText(delta string) error {
	if s.text == nil {
		s.text = &textState{ID: ids.New("msg"), OutputIndex: s.nextOutput}
		s.nextOutput++
		if s.Writer != nil {
			item := map[string]any{"id": s.text.ID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
			if err := s.Writer.Send("response.output_item.added", map[string]any{"output_index": s.text.OutputIndex, "item": item}); err != nil {
				return err
			}
			part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}
			if err := s.Writer.Send("response.content_part.added", map[string]any{"item_id": s.text.ID, "output_index": s.text.OutputIndex, "content_index": 0, "part": part}); err != nil {
				return err
			}
		}
	}
	s.text.Text.WriteString(delta)
	if s.Writer != nil {
		return s.Writer.Send("response.output_text.delta", map[string]any{"item_id": s.text.ID, "output_index": s.text.OutputIndex, "content_index": 0, "delta": delta, "logprobs": []any{}})
	}
	return nil
}

func (s *OutputState) addToolDelta(d chat.ToolCallDelta) error {
	t := s.tools[d.Index]
	if t == nil {
		t = &toolState{ItemID: ids.New("fc"), OutputIndex: -1}
		s.tools[d.Index] = t
	}
	if d.ID != "" {
		t.CallID = d.ID
	}
	if d.Function.Name != "" {
		t.Name.WriteString(d.Function.Name)
	}
	if d.Function.Arguments != "" {
		t.Arguments.WriteString(d.Function.Arguments)
	}
	if t.Kind == "" {
		t.Kind = s.toolKinds[t.Name.String()]
	}
	if !t.Added && t.Kind != "" && (t.CallID != "" || t.Name.Len() > 0) {
		if t.CallID == "" {
			t.CallID = ids.New("call")
		}
		t.OutputIndex = s.nextOutput
		s.nextOutput++
		t.Added = true
		if s.Writer != nil {
			item := s.toolItem(t, "in_progress", "")
			if err := s.Writer.Send("response.output_item.added", map[string]any{"output_index": t.OutputIndex, "item": item}); err != nil {
				return err
			}
		}
	}
	if t.Added && t.Kind != "custom" && t.Arguments.Len() > t.EmittedArgs && s.Writer != nil {
		all := t.Arguments.String()
		delta := all[t.EmittedArgs:]
		t.EmittedArgs = len(all)
		return s.Writer.Send("response.function_call_arguments.delta", map[string]any{"item_id": t.ItemID, "output_index": t.OutputIndex, "delta": delta})
	}
	return nil
}

func (s *OutputState) Complete() (map[string]any, error) {
	if err := s.Start(); err != nil {
		return nil, err
	}
	if s.text != nil && s.Writer != nil {
		full := s.text.Text.String()
		if err := s.Writer.Send("response.output_text.done", map[string]any{"item_id": s.text.ID, "output_index": s.text.OutputIndex, "content_index": 0, "text": full, "logprobs": []any{}}); err != nil {
			return nil, err
		}
		part := map[string]any{"type": "output_text", "text": full, "annotations": []any{}, "logprobs": []any{}}
		if err := s.Writer.Send("response.content_part.done", map[string]any{"item_id": s.text.ID, "output_index": s.text.OutputIndex, "content_index": 0, "part": part}); err != nil {
			return nil, err
		}
		if err := s.Writer.Send("response.output_item.done", map[string]any{"output_index": s.text.OutputIndex, "item": s.textItem("completed")}); err != nil {
			return nil, err
		}
	}
	indices := make([]int, 0, len(s.tools))
	for i := range s.tools {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		t := s.tools[i]
		if t.Kind == "" {
			t.Kind = s.toolKinds[t.Name.String()]
			if t.Kind == "" {
				t.Kind = "function"
			}
		}
		if !t.Added {
			if t.CallID == "" {
				t.CallID = ids.New("call")
			}
			t.OutputIndex, t.Added = s.nextOutput, true
			s.nextOutput++
			if s.Writer != nil {
				if err := s.Writer.Send("response.output_item.added", map[string]any{"output_index": t.OutputIndex, "item": s.toolItem(t, "in_progress", "")}); err != nil {
					return nil, err
				}
			}
		}
		if s.Writer != nil && t.Kind == "custom" {
			input := customInput(t.Arguments.String())
			if input != "" {
				if err := s.Writer.Send("response.custom_tool_call_input.delta", map[string]any{"item_id": t.ItemID, "output_index": t.OutputIndex, "delta": input}); err != nil {
					return nil, err
				}
			}
			if err := s.Writer.Send("response.custom_tool_call_input.done", map[string]any{"item_id": t.ItemID, "output_index": t.OutputIndex, "input": input}); err != nil {
				return nil, err
			}
			if err := s.Writer.Send("response.output_item.done", map[string]any{"output_index": t.OutputIndex, "item": s.toolItem(t, "completed", input)}); err != nil {
				return nil, err
			}
		} else if s.Writer != nil {
			if t.Arguments.Len() > t.EmittedArgs {
				all := t.Arguments.String()
				delta := all[t.EmittedArgs:]
				t.EmittedArgs = len(all)
				if err := s.Writer.Send("response.function_call_arguments.delta", map[string]any{"item_id": t.ItemID, "output_index": t.OutputIndex, "delta": delta}); err != nil {
					return nil, err
				}
			}
			if err := s.Writer.Send("response.function_call_arguments.done", map[string]any{"item_id": t.ItemID, "output_index": t.OutputIndex, "arguments": t.Arguments.String()}); err != nil {
				return nil, err
			}
			if err := s.Writer.Send("response.output_item.done", map[string]any{"output_index": t.OutputIndex, "item": s.toolItem(t, "completed", t.Arguments.String())}); err != nil {
				return nil, err
			}
		}
	}
	status, event := "completed", "response.completed"
	if s.FinishReason == "length" {
		status, event = "incomplete", "response.incomplete"
	} else if s.FinishReason == "error" {
		status, event = "failed", "response.failed"
	}
	resp := s.response(status, true)
	if status == "failed" {
		resp["error"] = map[string]any{"code": "upstream_error", "message": "upstream model reported an error finish reason"}
	}
	if s.Writer != nil {
		if err := s.Writer.Send(event, map[string]any{"response": resp}); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func (s *OutputState) Fail(code, message string) error {
	if !s.Started {
		if err := s.Start(); err != nil {
			return err
		}
	}
	if s.Writer == nil {
		return nil
	}
	resp := s.response("failed", true)
	resp["error"] = map[string]any{"code": code, "message": message}
	return s.Writer.Send("response.failed", map[string]any{"response": resp})
}

func (s *OutputState) response(status string, final bool) map[string]any {
	out := []any{}
	if final {
		items := make([]struct {
			index int
			value any
		}, 0, 1+len(s.tools))
		if s.text != nil {
			items = append(items, struct {
				index int
				value any
			}{s.text.OutputIndex, s.textItem("completed")})
		}
		for _, t := range s.tools {
			if t.Added {
				items = append(items, struct {
					index int
					value any
				}{t.OutputIndex, s.toolItem(t, "completed", s.toolArguments(t))})
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i].index < items[j].index })
		for _, item := range items {
			out = append(out, item.value)
		}
	}
	tools := any([]any{})
	if len(s.Tools) > 0 {
		tools = s.Tools
	}
	r := map[string]any{
		"id": s.ResponseID, "object": "response", "created_at": s.CreatedAt, "status": status, "model": s.Model, "output": out,
		"error": nil, "incomplete_details": nil, "instructions": nil, "metadata": map[string]any{}, "parallel_tool_calls": s.ParallelToolCalls,
		"temperature": nil, "tool_choice": s.ToolChoice, "tools": tools, "top_p": nil,
	}
	if status == "incomplete" {
		r["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if s.Usage != nil {
		u := map[string]any{"input_tokens": s.Usage.PromptTokens, "output_tokens": s.Usage.CompletionTokens, "total_tokens": s.Usage.TotalTokens}
		if len(s.Usage.CompletionDetail) > 0 {
			var details any
			if json.Unmarshal(s.Usage.CompletionDetail, &details) == nil {
				u["output_tokens_details"] = details
			}
		}
		r["usage"] = u
	}
	return r
}

func (s *OutputState) textItem(status string) map[string]any {
	full := s.text.Text.String()
	return map[string]any{"id": s.text.ID, "type": "message", "status": status, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": full, "annotations": []any{}, "logprobs": []any{}}}}
}

func (s *OutputState) toolItem(t *toolState, status, args string) map[string]any {
	if t.Kind == "custom" {
		return map[string]any{"id": t.ItemID, "type": "custom_tool_call", "call_id": t.CallID, "name": t.Name.String(), "input": args, "status": status}
	}
	return map[string]any{"id": t.ItemID, "type": "function_call", "call_id": t.CallID, "name": t.Name.String(), "arguments": args, "status": status}
}

func (s *OutputState) toolArguments(t *toolState) string {
	if t.Kind == "custom" {
		return customInput(t.Arguments.String())
	}
	return t.Arguments.String()
}

func customInput(arguments string) string {
	var wrapped struct {
		Input *string `json:"input"`
		Value *string `json:"value"`
	}
	if json.Unmarshal([]byte(arguments), &wrapped) == nil {
		if wrapped.Value != nil {
			return *wrapped.Value
		}
		if wrapped.Input != nil {
			return *wrapped.Input
		}
	}
	return arguments
}
