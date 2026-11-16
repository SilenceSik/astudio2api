package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatToResponsesBasic(t *testing.T) {
	body := map[string]any{
		"model": "glm-5.2",
		"messages": []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "user", "content": "你好"},
		},
		"max_tokens": float64(64),
		"stream":     true,
	}
	out := chatToResponses(body)
	if out["model"] != "glm-5.2" {
		t.Fatalf("model not carried: %v", out["model"])
	}
	if out["instructions"] != "你是助手" {
		t.Fatalf("system message should become instructions, got %v", out["instructions"])
	}
	if out["max_output_tokens"] != float64(64) {
		t.Fatalf("max_tokens should map to max_output_tokens")
	}
	input, _ := out["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("system message must not stay in input, got %d items", len(input))
	}
	first, _ := input[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "你好" {
		t.Fatalf("unexpected input item: %v", first)
	}
}

func TestChatToResponsesToolCall(t *testing.T) {
	body := map[string]any{
		"model": "glm-5.2",
		"messages": []any{
			map[string]any{"role": "user", "content": "查天气"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function",
					"function": map[string]any{"name": "weather", "arguments": "{\"city\":\"北京\"}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "晴"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "weather", "description": "查天气",
			"parameters": map[string]any{"type": "object"},
		}}},
	}
	out := chatToResponses(body)
	input, _ := out["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("expected 3 input items, got %d", len(input))
	}
	if input[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("assistant tool call should become function_call")
	}
	if input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("tool message should become function_call_output")
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].(map[string]any)["name"] != "weather" {
		t.Fatalf("tool should be flattened, got %v", tools[0])
	}
}

func TestResponsesToChatText(t *testing.T) {
	raw := []byte(`{"id":"cht123","model":"xopglm52","created_at":1794839748,
	  "output":[{"type":"message","content":[{"type":"output_text","text":"你好"}]}],
	  "usage":{"input_tokens":9,"output_tokens":3}}`)
	out, err := responsesToChat(raw, "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	choices := got["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Fatalf("content mismatch: %v", msg["content"])
	}
	usage := got["usage"].(map[string]any)
	if usage["total_tokens"] != float64(12) {
		t.Fatalf("usage sum wrong: %v", usage["total_tokens"])
	}
}

func TestResponsesToChatFunctionCall(t *testing.T) {
	raw := []byte(`{"id":"cht456","model":"xopglm52",
	  "output":[{"type":"function_call","call_id":"call_9","name":"weather","arguments":"{\"city\":\"北京\"}"}],
	  "usage":{"input_tokens":5,"output_tokens":2}}`)
	out, err := responsesToChat(raw, "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason should be tool_calls, got %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("expected one tool call")
	}
}

func TestStreamResponsesToChat(t *testing.T) {
	sse := strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"你"}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"好"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed"}`,
		"",
	}, "\n")
	var b strings.Builder
	if err := streamResponsesToChat(strings.NewReader(sse), "glm-5.2", &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, `"content":"你"`) || !strings.Contains(out, `"content":"好"`) {
		t.Fatalf("deltas not converted: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("completion event should close with finish_reason: %s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "[DONE]") {
		t.Fatalf("missing [DONE]: %s", out)
	}
}