package main

// Responses API 适配层。
//
// 一部分讯飞账号（Astron Coding Plan 订购）只在 Responses 通道上有权限：
//   https://astronstudio-api-volces-prod.xf-yun.com/model-gateway/v1/responses
// 而 MaaS 订购的账号走 chat/completions。对外我们只暴露 chat 协议，
// 所以这里把 chat 请求转成 responses 请求，再把响应（含流式）转回 chat 形状。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// contentText 把 chat 的 content（字符串或多模态数组）压成纯文本。
func contentText(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := m["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	default:
		return fmt.Sprint(c)
	}
}

// chatToResponses 把 OpenAI chat 请求体转成 Responses 请求体。
func chatToResponses(body map[string]any) map[string]any {
	out := map[string]any{}
	if v, ok := body["model"]; ok {
		out["model"] = v
	}
	var instructions []string
	var input []any

	msgs, _ := body["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		text := contentText(m["content"])

		switch role {
		case "system", "developer":
			if text != "" {
				instructions = append(instructions, text)
			}
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m["tool_call_id"],
				"output":  text,
			})
		case "assistant":
			if calls, ok := m["tool_calls"].([]any); ok {
				for _, rc := range calls {
					tc, _ := rc.(map[string]any)
					if tc == nil {
						continue
					}
					fn, _ := tc["function"].(map[string]any)
					if fn == nil {
						continue
					}
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   tc["id"],
						"name":      fn["name"],
						"arguments": fn["arguments"],
					})
				}
			}
			if text != "" {
				input = append(input, map[string]any{"role": "assistant", "content": text})
			}
		default:
			if text != "" {
				input = append(input, map[string]any{"role": role, "content": text})
			}
		}
	}
	out["input"] = input
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n")
	}
	if v, ok := body["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	} else if v, ok := body["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	for _, k := range []string{"stream", "temperature", "top_p", "store"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	if ts, ok := body["tools"].([]any); ok && len(ts) > 0 {
		var tools []any
		for _, raw := range ts {
			t, _ := raw.(map[string]any)
			fn, _ := t["function"].(map[string]any)
			if fn == nil {
				continue
			}
			tool := map[string]any{"type": "function", "name": fn["name"]}
			if v, ok := fn["description"]; ok {
				tool["description"] = v
			}
			if v, ok := fn["parameters"]; ok {
				tool["parameters"] = v
			}
			tools = append(tools, tool)
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	return out
}

// responsesToChat 把非流式 Responses 响应翻成 chat.completion。
func responsesToChat(raw []byte, requestedModel string) ([]byte, error) {
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	model, _ := resp["model"].(string)
	if model == "" {
		model = requestedModel
	}
	var text strings.Builder
	var toolCalls []any

	if out, ok := resp["output"].([]any); ok {
		for _, rawItem := range out {
			item, _ := rawItem.(map[string]any)
			if item == nil {
				continue
			}
			switch item["type"] {
			case "message":
				if parts, ok := item["content"].([]any); ok {
					for _, rawPart := range parts {
						part, _ := rawPart.(map[string]any)
						if part == nil {
							continue
						}
						if t, ok := part["text"].(string); ok {
							text.WriteString(t)
						}
					}
				}
			case "function_call":
				args := ""
				if v, ok := item["arguments"].(string); ok {
					args = v
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   item["call_id"],
					"type": "function",
					"function": map[string]any{
						"name":      item["name"],
						"arguments": args,
					},
				})
			}
		}
	}

	msg := map[string]any{"role": "assistant", "content": text.String()}
	finish := "stop"
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		msg["content"] = nil
		finish = "tool_calls"
	}

	usage := map[string]any{}
	if u, ok := resp["usage"].(map[string]any); ok {
		in := u["input_tokens"]
		outTokens := u["output_tokens"]
		usage["prompt_tokens"] = in
		usage["completion_tokens"] = outTokens
		usage["total_tokens"] = sumNumbers(in, outTokens)
	}

	id, _ := resp["id"].(string)
	created := time.Now().Unix()
	if v, ok := resp["created_at"].(float64); ok {
		created = int64(v)
	}
	out := map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
	return json.Marshal(out)
}

func sumNumbers(a, b any) any {
	fa, oka := a.(float64)
	fb, okb := b.(float64)
	if oka && okb {
		return fa + fb
	}
	return nil
}

// streamResponsesToChat 把 Responses 的 SSE 流转成 chat.completion.chunk 流。
func streamResponsesToChat(src io.Reader, model string, w io.Writer) error {
	reader := bufio.NewReaderSize(src, 64*1024)
	flusher, _ := w.(http.Flusher)
	created := time.Now().Unix()
	chatID := fmt.Sprintf("chatcmpl-%d", created)
	sentRole := false
	toolIndex := -1

	emit := func(delta map[string]any, finish any) error {
		if !sentRole {
			if _, ok := delta["role"]; !ok {
				delta["role"] = "assistant"
			}
			sentRole = true
		}
		chunk := map[string]any{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		b, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload != "" && payload != "[DONE]" {
					var ev map[string]any
					if jerr := json.Unmarshal([]byte(payload), &ev); jerr == nil {
						switch ev["type"] {
						case "response.output_text.delta":
							if d, ok := ev["delta"].(string); ok && d != "" {
								if eerr := emit(map[string]any{"content": d}, nil); eerr != nil {
									return eerr
								}
							}
						case "response.output_item.added":
							item, _ := ev["item"].(map[string]any)
							if item != nil && item["type"] == "function_call" {
								toolIndex++
								delta := map[string]any{"tool_calls": []any{map[string]any{
									"index": toolIndex,
									"id":    item["call_id"],
									"type":  "function",
									"function": map[string]any{
										"name":      item["name"],
										"arguments": "",
									},
								}}}
								if eerr := emit(delta, nil); eerr != nil {
									return eerr
								}
							}
						case "response.function_call_arguments.delta":
							if d, ok := ev["delta"].(string); ok && d != "" && toolIndex >= 0 {
								delta := map[string]any{"tool_calls": []any{map[string]any{
									"index":    toolIndex,
									"function": map[string]any{"arguments": d},
								}}}
								if eerr := emit(delta, nil); eerr != nil {
									return eerr
								}
							}
						case "response.completed":
							finish := "stop"
							if toolIndex >= 0 {
								finish = "tool_calls"
							}
							if eerr := emit(map[string]any{}, finish); eerr != nil {
								return eerr
							}
						case "response.failed", "error":
							msg := "upstream responses error"
							if m, ok := ev["message"].(string); ok && m != "" {
								msg = m
							}
							if _, werr := fmt.Fprintf(w, "data: {\"error\":{\"message\":%q}}\n\n", msg); werr != nil {
								return werr
							}
							if flusher != nil {
								flusher.Flush()
							}
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// chatViaResponses 用 Responses 通道完成一次 chat 请求，返回 chat 形状的响应。
func (u *Upstream) chatViaResponses(ctx context.Context, body map[string]any) (*http.Response, error) {
	conv := chatToResponses(body)
	mid := fmt.Sprint(body["model"])
	base := u.responsesBaseURL()
	url := strings.TrimRight(base, "/") + "/responses"
	key := u.account.Bearer()

	resp, err := u.doJSON(ctx, url, key, conv)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return resp, nil
	}
	stream, _ := body["stream"].(bool)
	if !stream {
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		out, cerr := responsesToChat(raw, mid)
		if cerr != nil {
			return nil, cerr
		}
		converted := &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(out)),
		}
		return converted, nil
	}
	pr, pw := io.Pipe()
	go func() {
		defer resp.Body.Close()
		defer pw.Close()
		_ = streamResponsesToChat(resp.Body, mid, pw)
	}()
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}, nil
}

// responsesBaseURL 账号级 Responses 地址，缺省用 Coding Plan 的 model-gateway。
func (u *Upstream) responsesBaseURL() string {
	if v := u.account.str("responsesBaseURL"); v != "" {
		return v
	}
	return "https://astronstudio-api-volces-prod.xf-yun.com/model-gateway/v1"
}

// responsesMode 账号是否走 Responses 通道。
func (u *Upstream) responsesMode() bool {
	return strings.EqualFold(u.account.str("upstreamMode"), "responses")
}