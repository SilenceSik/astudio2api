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

// foldToolCall 把一轮工具调用压成一行文本，用于超过上游 9 个调用上限时折叠旧历史。
func foldToolCall(fn map[string]any, output string) string {
	name := asString(fn["name"])
	args := asString(fn["arguments"])
	if len([]rune(args)) > 200 {
		args = string([]rune(args)[:200]) + "…"
	}
	if len([]rune(output)) > 300 {
		output = string([]rune(output)[:300]) + "…"
	}
	return "- " + name + " " + args + " → " + output
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
//
// rewrite 可选（见 idvault.go）：把客户端的 call_id 换发成本站签发过的真 id。
// 上游对 function_call.call_id 做的是「查库」，外厂 id 会被 400 拒绝；
// 而它不校验 name/arguments，所以换发不会破坏工具语义。
func chatToResponses(body map[string]any, rewrite ...func(string) string) map[string]any {
	rw := firstStringFunc(rewrite)
	out := map[string]any{}
	if v, ok := body["model"]; ok {
		out["model"] = v
	}
	var instructions []string
	var input []any

	msgs := anySlice(body["messages"])

	// 本条上游要求「function_call 紧跟它的 function_call_output」成对出现。
	// 实测（2026-11-17）：把一轮里的多个 function_call 攒在一起、再统一给 outputs
	// 会 400 invalid_prompt；交错排放才是 200。所以这里先把工具结果按 id 建索引，
	// 遇到 assistant 的调用就与其结果就地配对输出。
	toolOutputs := map[string]string{}
	toolImages := map[string][]any{}
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if m["role"] != "tool" {
			continue
		}
		id := asString(m["tool_call_id"])
		if id == "" {
			continue
		}
		if _, exists := toolOutputs[id]; exists {
			continue
		}
		parts, text := contentParts(m["content"])
		toolOutputs[id] = text
		if parts != nil {
			toolImages[id] = parts
		}
	}
	emitted := map[string]bool{}

	// 上游硬限：input 里最多 9 个 function_call 项（实测第 10 个就 400 invalid_prompt）。
	// 超过就把最旧的若干轮折成一条文本摘要，保留最近 9 个原样回放。
	const maxToolCalls = 9
	var callOrder []string
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "assistant" {
			continue
		}
		for _, rc := range anySlice(m["tool_calls"]) {
			if tc, ok := rc.(map[string]any); ok {
				if id := asString(tc["id"]); id != "" {
					callOrder = append(callOrder, id)
				}
			}
		}
	}
	keep := map[string]bool{}
	if len(callOrder) > maxToolCalls {
		for _, id := range callOrder[len(callOrder)-maxToolCalls:] {
			keep[id] = true
		}
	} else {
		for _, id := range callOrder {
			keep[id] = true
		}
	}
	var folded []string
	foldedFlushed := false
	flushFolded := func() {
		if foldedFlushed || len(folded) == 0 {
			return
		}
		foldedFlushed = true
		input = append(input, map[string]any{
			"role": "user",
			"content": "（以下是更早的工具调用记录，已折叠成文本以免超过上游限制）\n" +
				strings.Join(folded, "\n"),
		})
	}

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
			// 结果已经在 assistant 那一侧就地配对输出；这里只剩孤儿结果
			// （找不到对应调用）—— 上游会拒，直接丢掉。
		case "assistant":
			if text != "" || parts != nil {
				item := map[string]any{"role": "assistant"}
				if parts != nil {
					item["content"] = parts
				} else {
					item["content"] = text
				}
				input = append(input, item)
			}
			for _, rc := range anySlice(m["tool_calls"]) {
				tc, _ := rc.(map[string]any)
				if tc == nil {
					continue
				}
				fn, _ := tc["function"].(map[string]any)
				if fn == nil {
					continue
				}
				id := asString(tc["id"])
				if !keep[id] {
					folded = append(folded, foldToolCall(fn, toolOutputs[id]))
					continue
				}
				flushFolded()
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   rw(id),
					"name":      fn["name"],
					"arguments": fn["arguments"],
				})
				if outText, has := toolOutputs[id]; has && !emitted[id] {
					emitted[id] = true
					input = append(input, map[string]any{
						"type":    "function_call_output",
						"call_id": rw(id),
						"output":  outText,
					})
					// 工具结果里的图片：上游不认 function_call_output 内部的
					// input_image（实测收下但不喂给模型），官方客户端也是另起一条
					// role=user 的消息装图。照它的做法走。
					if imgs := toolImages[id]; len(imgs) > 0 {
						input = append(input, map[string]any{
							"role":    "user",
							"content": imgs,
						})
					}
				}
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
	flushFolded()
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
	// ⚠️ 实测（2026-11-17）：这条上游只认 "auto" / "none" / 缺省，
	// 传 "required" 或对象形式一律 400 invalid_prompt。所以只透传它认的两种，
	// 其余丢弃 —— 丢掉只会让工具调用变成「可选」，比整轮 400 划算。
	if tc, ok := body["tool_choice"]; ok {
		if s, ok := tc.(string); ok && (s == "auto" || s == "none") {
			out["tool_choice"] = s
		}
	}
	if v, ok := body["parallel_tool_calls"]; ok {
		out["parallel_tool_calls"] = v
	}
	return out
}

// responsesToChat 把非流式 Responses 响应翻成 chat.completion。
// harvest 可选：把上游签发的 call_id 记进台账，供后续换发。
func responsesToChat(raw []byte, requestedModel string, harvest ...func(string)) ([]byte, error) {
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
				if id := asString(item["call_id"]); id != "" {
					firstStringHook(harvest)(id)
				}
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
		if conv := usageFromResponsesUsage(u); conv != nil {
			usage = conv
		}
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

// numOf 把 JSON 数字（float64 / json.Number / int）统一成 float64。
func numOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f
		}
	}
	return 0
}

// usageFromResponsesUsage 把 Responses 的 usage 翻成 chat 形状，保留缓存与思考明细。
//
// 上游在 response.completed 里给出真实用量：input_tokens / output_tokens，
// 以及 input_tokens_details.cached_tokens（缓存命中）和
// output_tokens_details.reasoning_tokens。不转发这几个字段，new-api 只能本地
// 估算，渠道统计里就会出现「没有缓存」的假象。
func usageFromResponsesUsage(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	in := numOf(u["input_tokens"])
	out := numOf(u["output_tokens"])
	usage := map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      in + out,
	}
	if d, ok := u["input_tokens_details"].(map[string]any); ok {
		if cached := numOf(d["cached_tokens"]); cached > 0 {
			usage["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}
	if d, ok := u["output_tokens_details"].(map[string]any); ok {
		if reasoning := numOf(d["reasoning_tokens"]); reasoning > 0 {
			usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
		}
	}
	return usage
}

// responsesUsageToChat 从 response.completed / response.incomplete 事件里取用量。
func responsesUsageToChat(ev map[string]any) map[string]any {
	resp, _ := ev["response"].(map[string]any)
	if resp == nil {
		return nil
	}
	u, _ := resp["usage"].(map[string]any)
	return usageFromResponsesUsage(u)
}

// streamResponsesToChat 把 Responses 的 SSE 流转成 chat.completion.chunk 流。
// harvest 可选：把上游签发的 call_id 记进台账（流式下每个 function_call 都会给）。
func streamResponsesToChat(src io.Reader, model string, w io.Writer, harvest ...func(string)) error {
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
								if id := asString(item["call_id"]); id != "" {
									firstStringHook(harvest)(id)
								}
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
							// 上游把真实用量（含缓存命中）放在这里；不转发它，
							// new-api 只能本地估算，渠道上就显示「没有缓存」。
							if chatUsage := responsesUsageToChat(ev); chatUsage != nil {
								final := map[string]any{
									"id":      chatID,
									"object":  "chat.completion.chunk",
									"created": created,
									"model":   model,
									"choices": []any{},
									"usage":   chatUsage,
								}
								if b, merr := json.Marshal(final); merr == nil {
									if _, werr := fmt.Fprintf(w, "data: %s\n\n", b); werr != nil {
										return werr
									}
									if flusher != nil {
										flusher.Flush()
									}
								}
							}
						case "response.failed", "error":
							msg := upstreamStreamErrorDetail(ev)
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
	scope := scopeOf(u.account.Name, mid)
	rewrite, harvest := u.toolIDBridge(ctx, body, scope, mid)
	conv := chatToResponses(body, rewrite)
	base := u.responsesBaseURL()
	url := strings.TrimRight(base, "/") + "/responses"
	// 优先用客户端 JWT：它才是 model-gateway 认的凭证；
	// configs 下发的 api_key 只在 MaaS（chat）通道有效。
	key := u.account.JWT()
	if key == "" {
		key = u.account.Bearer()
	}

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
		out, cerr := responsesToChat(raw, mid, harvest)
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
	// 先读上游流的第一条事件：上游会在开始生成前就把校验错误以
	// event: response.failed 发出来。若不在写响应头之前拦住，我们只能回
	// 「200 + 一帧错误」，而 new-api 不认这种帧 —— 记成空回成功，
	// 既不降温也不换渠道。所以这里提前判一次，把错误变成真 HTTP 400。
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	guard := time.AfterFunc(30*time.Second, func() { _ = resp.Body.Close() })
	consumed, first, detail, perr := peekFirstEvent(br)
	guard.Stop()
	if perr != nil {
		resp.Body.Close()
		return nil, perr
	}
	if first == "failed" {
		resp.Body.Close()
		return plainErrorResponse(http.StatusBadRequest, detail), nil
	}

	pr, pw := io.Pipe()
	go func() {
		defer resp.Body.Close()
		defer pw.Close()
		_ = streamResponsesToChat(io.MultiReader(bytes.NewReader(consumed), br), mid, pw, harvest)
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

// ---------------------------------------------------------------------------
// 工具调用 id 换发（详见 idvault.go）

func firstStringFunc(fns []func(string) string) func(string) string {
	if len(fns) > 0 && fns[0] != nil {
		return fns[0]
	}
	return func(id string) string { return id }
}

func firstStringHook(fns []func(string)) func(string) {
	if len(fns) > 0 && fns[0] != nil {
		return fns[0]
	}
	return func(string) {}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// upstreamStreamErrorDetail 尽量把上游错误原文取出来。
// 讯飞把错误包在 event: response.failed 的 response.error 里；
// 只报一句 "upstream responses error" 会让排障时看不到真因。
func upstreamStreamErrorDetail(ev map[string]any) string {
	msg := asString(ev["message"])
	if resp, ok := ev["response"].(map[string]any); ok {
		if e, ok := resp["error"].(map[string]any); ok {
			detail := asString(e["message"])
			code := asString(e["code"])
			if detail != "" && code != "" {
				msg = code + ": " + detail
			} else if detail != "" {
				msg = detail
			} else if code != "" {
				msg = code
			}
		}
	}
	if e, ok := ev["error"].(map[string]any); ok {
		if detail := asString(e["message"]); detail != "" {
			msg = detail
		}
	}
	if msg == "" {
		msg = "upstream responses error"
	}
	return msg
}

// peekFirstEvent 读上游 SSE 直到第一条 data 事件，返回已读字节与判定。
// 已读字节要原样交给下游转换器，不能丢——丢了就少一帧。
// 判定 "failed" 表示这条流一上来就是错误事件，调用方应当直接回 4xx。
func peekFirstEvent(br *bufio.Reader) ([]byte, string, string, error) {
	var buf bytes.Buffer
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			buf.Write(line)
			trimmed := strings.TrimSpace(string(line))
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload != "" && payload != "[DONE]" {
					var ev map[string]any
					if json.Unmarshal([]byte(payload), &ev) == nil {
						switch ev["type"] {
						case "response.failed", "error":
							return buf.Bytes(), "failed", upstreamStreamErrorDetail(ev), nil
						}
					}
				}
				return buf.Bytes(), "event", "", nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return buf.Bytes(), "event", "", nil
			}
			return buf.Bytes(), "", "", err
		}
	}
}

// toolIDBridge 给本轮请求建立 id 换发表，并返回收割回调。
//
// 流程：先按粘性映射分配 -> 缺货就 prime 领真 id -> 还不够就借最近一个真 id。
// 池子空、prime 也失败时退化为原样透传（让上游自己报 400），不在这里打死请求。
func (u *Upstream) toolIDBridge(ctx context.Context, body map[string]any, scope, mid string) (func(string) string, func(string)) {
	v := getIDVault()
	harvest := func(string) {}
	if v == nil {
		return nil, harvest
	}
	harvest = func(id string) { v.Harvest(scope, id) }

	clients := collectToolCallIDs(body)
	if len(clients) == 0 {
		return nil, harvest
	}
	mapping, missing := v.Assign(scope, clients)
	for attempt := 0; attempt < 2 && len(missing) > 0; attempt++ {
		if err := u.primeCallID(ctx, mid, scope); err != nil {
			logf("[%s] prime 真 call_id 失败: %v", u.account.Name, err)
			break
		}
		mapping, missing = v.Assign(scope, clients)
	}
	if len(missing) > 0 {
		mapping = v.Backfill(scope, missing, mapping)
		logf("[%s] 台账缺货 %d 个 id，已借用最近真 id 兜底", u.account.Name, len(missing))
	}
	rewrite := func(id string) string {
		if real, ok := mapping[id]; ok && real != "" {
			return real
		}
		return id
	}
	return rewrite, harvest
}

// primeCallID 向上游领一个真 call_id。
//
// 上游只在它自己发 function_call 时才签发 id，所以这里发一条最小请求，
// 用 tool_choice 强制它调用哑工具，再从返回体里取 call_id。
func (u *Upstream) primeCallID(ctx context.Context, mid, scope string) error {
	probe := map[string]any{
		"model": mid,
		"input": []any{map[string]any{"role": "user", "content": "调用 astudio_probe，参数留空。"}},
		"tools": []any{map[string]any{
			"type":        "function",
			"name":        "astudio_probe",
			"description": "internal no-op probe",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		"tool_choice":       "auto",
		"max_output_tokens": 128,
		"store":             false,
		"stream":            false,
	}
	key := u.account.JWT()
	if key == "" {
		key = u.account.Bearer()
	}
	url := strings.TrimRight(u.responsesBaseURL(), "/") + "/responses"
	resp, err := u.doJSON(ctx, url, key, probe)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		return rerr
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	for _, item := range anySlice(out["output"]) {
		m, _ := item.(map[string]any)
		if m == nil || m["type"] != "function_call" {
			continue
		}
		if id := asString(m["call_id"]); id != "" {
			if v := getIDVault(); v != nil {
				v.Harvest(scope, id)
			}
			return nil
		}
	}
	return fmt.Errorf("返回体里没有 function_call")
}
