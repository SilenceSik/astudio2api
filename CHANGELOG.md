# 变更日志

本文件记录对外可见的行为变化。更早版本见 git 历史。

## 2.2.0

工具调用链路上的四个协议约束集中修复。它们此前都表现为「请求返回 200、但补全为空」，
因为上游把校验错误放在流式响应内部，而代理层只认标准的 choices/usage。

### 新增

- **工具调用 id 台账**（`go/idvault.go`）：按「账号 × 模型」记录上游签发过的 `call_id`，
  同一个客户端 `call_id` 粘性换发同一个真 id；台账缺货时以最小请求向上游领取，
  仍不足则借用最近一个真 id，保证请求不因台账原因失败。
- **真实用量转发**：流式路径补发 `choices` 为空的 usage 帧，保留 `prompt_tokens_details.cached_tokens`
  与 `completion_tokens_details.reasoning_tokens`。此前代理层只能本地估算，缓存命中恒显示为 0。
- **流式错误前置判定**：上游以 `event: response.failed` 报错时 HTTP 仍为 200，
  现在在写响应头之前判定并转为 `HTTP 400` 带上游原文，使上游错误能被正常记录与切换。

### 修复

- `function_call` 必须与其 `function_call_output` 相邻成对；一轮内多个调用时，
  结果需交错排列，攒齐后再统一给出会被上游拒绝。
- `input` 中 `function_call` 项上限为 9；超出部分折叠为文本摘要（`- 工具 参数 → 结果`），
  保留最近 9 个结构化回放。
- `tool_choice` 仅透传 `auto` / `none` / 缺省，其余取值会被上游拒绝。
- 工具结果中的图片改为追加一条 `role: user` 的多模态消息：上游不消费
  `function_call_output` 内部的 `input_image`，直接放在结果里模型看不到。
- 客户端标识 `studioVersion` 更新至 3.6.4；3.6.4 客户端仍以 `model-gateway/v1` 为推理入口。

### 升级提示

- 无需数据迁移。`idvault.json` 与 `state.json` 同目录，首次请求时自动建立。
- 冷启动阶段台账为空，前几轮会出现「借用最近真 id」的降级映射；随流量自然恢复一对一。