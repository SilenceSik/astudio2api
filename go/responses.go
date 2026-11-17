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

// contentParts 把 chat 的 content 转成 Responses 的 parts。
// 纯文本走字符串（简单可靠）；含图片时返回 input_text / input_image 数组。
// Responses 的 input_image 与 chat 的 image_url 结构相同，只需换 type 名。
func contentParts(v any) (parts []any, plain string) {
	arr := anySlice(v)
	if len(arr) == 0 {
		return nil, contentText(v)
	}
	var b strings.Builder
	hasImage := false
	for _, raw := range arr {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch p["type"] {
		case "text", "input_text":
			t, _ := p["text"].(string)
			b.WriteString(t)
			parts = append(parts, map[string]any{"type": "input_text", "text": t})
		case "image_url", "input_image":
			hasImage = true
			parts = append(parts, map[string]any{"type": "input_image", "image_url": p["image_url"]})
		}
	}
	if !hasImage {
		return nil, b.String()
	}
	return parts, b.String()
}

// anySlice 把任意 slice 形态统一成 []any。
// JSON 解码得到 []any，而 Go 内部调用方常直接构造 []map[string]any —— 只认前者会
// 让 messages/tools 被当成空（上游随即 400 invalid_prompt）。
func anySlice(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	case []map[string]any:
		out := make([]any, len(s))
		for i := range s {
			out[i] = s[i]
		}
		return out
	default:
		return nil
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

	msgs := anySlice(body["messages"])
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		parts, text := contentParts(m["content"])

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
			if calls := anySlice(m["tool_calls"]); len(calls) > 0 {
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
			if text != "" || parts != nil {
				item := map[string]any{"role": "assistant"}
				if parts != nil {
					item["content"] = parts
				} else {
					item["content"] = text
				}
				input = append(input, item)
			}
		default:
			if text != "" || parts != nil {
				item := map[string]any{"role": role}
				if parts != nil {
					item["content"] = parts
				} else {
					item["content"] = text
				}
				input = append(input, item)
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
	if ts := anySlice(body["tools"]); len(ts) > 0 {
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
	// tool_choice 形状不同：chat 是 {"type":"function","function":{"name":..}}，
	// responses 是 {"type":"function","name":..}；字符串枚举两边通用。
	if tc, ok := body["tool_choice"]; ok {
		switch t := tc.(type) {
		case string:
			out["tool_choice"] = t
		case map[string]any:
			if fn, ok := t["function"].(map[string]any); ok && fn["name"] != nil {
				out["tool_choice"] = map[string]any{"type": "function", "name": fn["name"]}
			}
		}
	}
	if v, ok := body["parallel_tool_calls"]; ok {
		out["parallel_tool_calls"] = v
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

	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	if u, ok := resp["usage"].(map[string]any); ok {
		in := u["input_tokens"]
		outTokens := u["output_tokens"]
		if in == nil {
			in = 0
		}
		if outTokens == nil {
			outTokens = 0
		}
		total := sumNumbers(in, outTokens)
		if total == nil {
			total = 0
		}
		usage["prompt_tokens"] = in
		usage["completion_tokens"] = outTokens
		usage["total_tokens"] = total
	}

	id, _ := resp["id"].(string)
	created := time.Now().Unix()
	if v, ok := resp["created_at"].(float64); ok {
		created = int64(v)
	}
	// 回显调用方传入的模型名；没有则退回上游返回的 model。
	// 上游返回的是内部代号（xopglm52），直接透出去会让客户端的模型名对不上。
	displayModel := requestedModel
	if displayModel == "" {
		displayModel = model
	}
	out := map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion",
		"created": created,
		"model":   displayModel,
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
						case "response.completed", "response.incomplete":
							finish := "stop"
							if toolIndex >= 0 {
								finish = "tool_calls"
							} else if ev["type"] == "response.incomplete" {
								// 被 max_output_tokens 截断：必须显式收尾，否则客户端见到的是空结束。
								finish = "length"
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

// hasImageInput 判断请求里是否带了图片输入（chat 的 image_url / responses 的 input_image）。
func hasImageInput(body map[string]any) bool {
	for _, raw := range anySlice(body["messages"]) {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, part := range anySlice(m["content"]) {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch p["type"] {
			case "image_url", "input_image", "image":
				return true
			}
		}
	}
	return false
}

// plainErrorResponse 构造一个只带 body 的错误响应，交给上层 errFromResponse 读。
func plainErrorResponse(status int, msg string) *http.Response {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": "upstream_error"},
	})
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
	}
}

// chatViaResponses 用 Responses 通道完成一次 chat 请求，返回 chat 形状的响应。
//
// 只有走 maas-api 的模型才受 Coding Plan 通道覆盖；同一个账号的 spark 系列模型
// 走 spark-api-open（自己的接口），把它们也送进 model-gateway 会 404。
func (u *Upstream) chatViaResponses(ctx context.Context, body map[string]any) (*http.Response, error) {
	mid := fmt.Sprint(body["model"])
	entry := u.Resolve(mid)
	if !codingPlanCovered(entry.BaseURL) {
		url, key := u.target(body, "/chat/completions")
		return u.doJSON(ctx, url, key, body)
	}
	conv := chatToResponses(body)
	base := u.responsesBaseURL()
	url := strings.TrimRight(base, "/") + "/responses"
	key := u.account.Bearer()

	resp, err := u.doJSON(ctx, url, key, conv)
	if err != nil {
		return nil, err
	}
	// model-gateway 不认的模型（如 spark 系）会回 404：退回该模型自己的 chat 通道再打一次。
	// 用状态码自适应，比维护一份「哪些模型受 Coding Plan 覆盖」的名单可靠。
	if resp.StatusCode == 404 {
		resp.Body.Close()
		chatURL, chatKey := u.target(body, "/chat/completions")
		return u.doJSON(ctx, chatURL, chatKey, body)
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

// codingPlanCovered 判断某个模型的 base_url 是否落在 Coding Plan 覆盖的通道上。
// configs 下发的 MaaS 地址（maas-api…）才是 Coding Plan 能接管的；
// spark-api-open 等其它通道有自己的 chat 接口，不能转 responses。
func codingPlanCovered(baseURL string) bool {
	return strings.Contains(baseURL, "maas-api.cn-huabei-1.xf-yun.com")
}

// responsesMode 账号是否走 Responses 通道。
func (u *Upstream) responsesMode() bool {
	return strings.EqualFold(u.account.str("upstreamMode"), "responses")
}
