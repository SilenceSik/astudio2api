package main

import (
	"bufio"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func newTestVault(t *testing.T) *idVault {
	t.Helper()
	dir := t.TempDir()
	v := newIDVault(filepath.Join(dir, "state.json"))
	setIDVault(v)
	t.Cleanup(func() { setIDVault(nil) })
	return v
}

// 外厂 id 必须换成本站签发的真 id，且同一个客户端 id 每次换到同一个真 id。
func TestVaultStickyOneToOne(t *testing.T) {
	v := newTestVault(t)
	scope := scopeOf("acct-a", "xdeepseek-flash-in")
	v.Harvest(scope, "call_00_real1")
	v.Harvest(scope, "call_00_real2")

	m1, miss1 := v.Assign(scope, []string{"call_ext_a", "call_ext_b"})
	if len(miss1) != 0 {
		t.Fatalf("should be satisfied from pool, missing=%v", miss1)
	}
	if m1["call_ext_a"] == m1["call_ext_b"] {
		t.Fatalf("distinct client ids must map to distinct real ids: %v", m1)
	}
	// 第二轮同一个客户端 id 必须粘回同一个真 id
	m2, _ := v.Assign(scope, []string{"call_ext_a", "call_ext_b"})
	if m2["call_ext_a"] != m1["call_ext_a"] || m2["call_ext_b"] != m1["call_ext_b"] {
		t.Fatalf("binding not sticky: first=%v second=%v", m1, m2)
	}
	// 池子用尽 -> 缺货
	_, miss2 := v.Assign(scope, []string{"call_ext_c"})
	if len(miss2) != 1 {
		t.Fatalf("expected one missing id, got %v", miss2)
	}
	// 兜底借最近一个真 id，不报错
	back := v.Backfill(scope, miss2, map[string]string{})
	if back["call_ext_c"] == "" {
		t.Fatalf("backfill should bind a real id")
	}
}

// 换发要落到转换后的 responses 报文上：function_call 与 output 用同一个真 id。
func TestChatToResponsesRewritesToolIDs(t *testing.T) {
	v := newTestVault(t)
	scope := scopeOf("acct-a", "xdeepseek-flash-in")
	v.Harvest(scope, "call_00_real1")

	body := map[string]any{
		"model": "xdeepseek-flash-in",
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_ext_1", "type": "function",
					"function": map[string]any{"name": "ping", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_ext_1", "content": "pong"},
		},
	}
	clients := collectToolCallIDs(body)
	if len(clients) != 1 || clients[0] != "call_ext_1" {
		t.Fatalf("collectToolCallIDs = %v", clients)
	}
	mapping, missing := v.Assign(scope, clients)
	if len(missing) != 0 {
		t.Fatalf("unexpected missing %v", missing)
	}
	out := chatToResponses(body, func(id string) string {
		if real, ok := mapping[id]; ok {
			return real
		}
		return id
	})
	input, _ := out["input"].([]any)
	var callID, outID string
	for _, it := range input {
		m, _ := it.(map[string]any)
		switch m["type"] {
		case "function_call":
			callID = asString(m["call_id"])
		case "function_call_output":
			outID = asString(m["call_id"])
		}
	}
	if callID != "call_00_real1" || outID != "call_00_real1" {
		t.Fatalf("rewrite not applied: call=%q output=%q", callID, outID)
	}
	// 参数与工具名必须原样保留 —— 上游只查 id，不校验它们
	if len(input) < 3 {
		t.Fatalf("input items lost: %v", out["input"])
	}
}

// 收割要落盘：重启后同一客户端 id 仍换到同一个真 id。
func TestVaultPersistsBindings(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	v1 := newIDVault(statePath)
	setIDVault(v1)
	scope := scopeOf("acct-a", "m")
	v1.Harvest(scope, "call_00_x")
	first, _ := v1.Assign(scope, []string{"call_ext"})

	v2 := newIDVault(statePath) // 模拟进程重启
	setIDVault(v2)
	t.Cleanup(func() { setIDVault(nil) })
	second, _ := v2.Assign(scope, []string{"call_ext"})
	if first["call_ext"] != second["call_ext"] {
		t.Fatalf("binding lost across restart: %v vs %v", first, second)
	}
	raw, err := json.Marshal(second)
	if err != nil || len(raw) == 0 {
		t.Fatalf("marshal sanity: %v", err)
	}
}

// 实测（2026-11-17）：本条上游要求 function_call 紧跟它的 output，
// 多个调用攒在一起再给结果会 400 invalid_prompt。转换层必须交错输出。
func TestChatToResponsesInterleavesPairs(t *testing.T) {
	body := map[string]any{
		"model": "xdeepseek-flash-in",
		"messages": []any{
			map[string]any{"role": "user", "content": "并行查两个"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}},
				map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "grep", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r1"},
			map[string]any{"role": "tool", "tool_call_id": "c2", "content": "r2"},
		},
	}
	out := chatToResponses(body)
	input, _ := out["input"].([]any)
	want := []string{"function_call", "function_call_output", "function_call", "function_call_output"}
	var got []string
	for _, it := range input {
		m, _ := it.(map[string]any)
		if s := asString(m["type"]); s != "" {
			got = append(got, s)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("item sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item sequence = %v, want %v", got, want)
		}
	}
	// 配对关系也要对：每个 function_call 后面紧跟同一个 call_id 的 output
	var typed []map[string]any
	for _, it := range input {
		m, _ := it.(map[string]any)
		if asString(m["type"]) != "" {
			typed = append(typed, m)
		}
	}
	for i := 0; i+1 < len(typed); i += 2 {
		if asString(typed[i]["call_id"]) != asString(typed[i+1]["call_id"]) {
			t.Fatalf("pair mismatch at %d: %v vs %v", i, typed[i]["call_id"], typed[i+1]["call_id"])
		}
	}
}

// 工具结果里的图片要单独补一条 role=user 的消息带图：
// 上游收下 function_call_output 内的 input_image 但不喂给模型（实测）。
func TestChatToResponsesToolImagesBecomeUserMessage(t *testing.T) {
	body := map[string]any{
		"model": "xdeepseek-flash-in",
		"messages": []any{
			map[string]any{"role": "user", "content": "截个图"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "shot", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": []any{
				map[string]any{"type": "text", "text": "截图如下"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
			}},
		},
	}
	out := chatToResponses(body)
	input, _ := out["input"].([]any)
	var sawImageMsg bool
	for _, it := range input {
		m, _ := it.(map[string]any)
		if asString(m["role"]) != "user" {
			continue
		}
		parts, _ := m["content"].([]any)
		for _, raw := range parts {
			p, _ := raw.(map[string]any)
			if asString(p["type"]) == "input_image" {
				sawImageMsg = true
			}
		}
	}
	if !sawImageMsg {
		t.Fatalf("tool image should become a user message with input_image: %v", input)
	}
}

// 上游在 response.completed 里给真实用量（含缓存命中），必须转发给 new-api，
// 否则渠道统计里永远是「没有缓存」。
func TestStreamForwardsUsageWithCache(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"收到"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":54035,"input_tokens_details":{"cached_tokens":53888},"output_tokens":16,"output_tokens_details":{"reasoning_tokens":14},"total_tokens":54051}}}`,
		``,
	}, "\n")
	var b strings.Builder
	if err := streamResponsesToChat(strings.NewReader(sse), "deepseek-v4.1-flash", &b); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, `"cached_tokens":53888`) {
		t.Fatalf("usage with cache not forwarded:\n%s", out)
	}
	if !strings.Contains(out, `"reasoning_tokens":14`) {
		t.Fatalf("reasoning tokens not forwarded:\n%s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":54035`) {
		t.Fatalf("prompt tokens not forwarded:\n%s", out)
	}
}

// 上游最多接受 9 个 function_call 项（第 10 个 400）。超限要把旧调用折成文本，
// 只保留最近 9 个结构化回放。
func TestChatToResponsesFoldsBeyondNineCalls(t *testing.T) {
	var msgs []any
	msgs = append(msgs, map[string]any{"role": "user", "content": "干活"})
	for i := 0; i < 12; i++ {
		id := "c" + string(rune('a'+i))
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}},
		}})
		msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": id, "content": "ok" + id})
	}
	out := chatToResponses(map[string]any{"model": "x", "messages": msgs})
	input, _ := out["input"].([]any)
	calls, folded := 0, false
	for _, it := range input {
		m, _ := it.(map[string]any)
		if asString(m["type"]) == "function_call" {
			calls++
		}
		if asString(m["role"]) == "user" {
			if c := asString(m["content"]); strings.Contains(c, "已折叠成文本") {
				folded = true
			}
		}
	}
	if calls != 9 {
		t.Fatalf("expected 9 kept calls, got %d", calls)
	}
	if !folded {
		t.Fatalf("old rounds should be folded into a text message")
	}
}

// 上游把校验错误放在流的开头（event: response.failed）。必须在写响应头之前
// 拦住并改成 4xx，否则 new-api 只看到 200 + 一帧错误，记成空回成功。
func TestPeekFirstEventDetectsFailure(t *testing.T) {
	sse := ": ping\n\nevent: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"invalid_prompt\",\"message\":\"invalid request\"}}}\n\n"
	consumed, kind, detail, err := peekFirstEvent(bufio.NewReaderSize(strings.NewReader(sse), 4096))
	if err != nil {
		t.Fatalf("peek err: %v", err)
	}
	if kind != "failed" {
		t.Fatalf("expected failed, got %q", kind)
	}
	if !strings.Contains(detail, "invalid_prompt") {
		t.Fatalf("detail should carry upstream code, got %q", detail)
	}
	if !strings.Contains(string(consumed), "response.failed") {
		t.Fatalf("consumed bytes must be preserved")
	}

	ok := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{}}\n\n"
	_, kind2, _, _ := peekFirstEvent(bufio.NewReaderSize(strings.NewReader(ok), 4096))
	if kind2 != "event" {
		t.Fatalf("created event should not be treated as failure, got %q", kind2)
	}
}
