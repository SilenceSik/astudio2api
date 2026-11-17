package main

// 工具调用 id 台账（call_id vault）。
//
// 背景（2026-11-17 实测）：astudio 的 model-gateway 对 function_call 的 call_id
// 做的是「查库/验签」，不是格式校验 —— 上游自己签发的 id 原样回放是 200，
// 新造一个同前缀同长度的是 400，真 id 改一个字符也是 400。
// 而 new-api 会把同一会话的下一轮轮转到别的渠道，工具历史里就混进了别家
// 签发的 call_id，落到本站的 deepseek-v4.1-flash 上必被 400 拒绝。
//
// 另一条实测：上游只认 id 在不在库，**不校验 name / arguments**
// （真 id 换成别的工具名、别的参数、甚至指向不存在的工具，全部 200），
// 所以把外厂 id 换发成本站签发过的 id 不会破坏工具语义。
//
// 这个文件负责三件事：
//  1. 收割：上游每次返回 function_call，就把它的 call_id 记进 (账号×模型) 池。
//  2. 粘性换发：同一个客户端 call_id 永远换同一个真 id（一对一），
//     不同客户端 id 各领各的，历史里的 id 序列看起来像真的。
//  3. 兜底：池子不够时先由调用方 prime，仍不够就借最近一个真 id，
//     不因为台账缺货把请求打死。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	idVaultFileName = "idvault.json"
	idVaultPoolCap  = 256
	idVaultTTL      = 24 * time.Hour
)

type idRecord struct {
	ID string `json:"id"`
	At int64  `json:"at"`
}

type idBinding struct {
	Real string `json:"real"`
	At   int64  `json:"at"`
}

type idVaultState struct {
	Version  int                   `json:"version"`
	Pools    map[string][]idRecord `json:"pools"`
	Bindings map[string]idBinding  `json:"bindings"`
}

type idVault struct {
	mu     sync.Mutex
	path   string
	loaded bool
	st     idVaultState
}

var activeVault *idVault

func setIDVault(v *idVault) { activeVault = v }

func getIDVault() *idVault { return activeVault }

// newIDVault 台账与 state.json 同目录，避免和账号目录混在一起。
func newIDVault(statePath string) *idVault {
	return &idVault{
		path: filepath.Join(filepath.Dir(statePath), idVaultFileName),
		st: idVaultState{
			Version:  1,
			Pools:    map[string][]idRecord{},
			Bindings: map[string]idBinding{},
		},
	}
}

// scopeOf 台账分区键：账号 × 模型。实测 id 不能跨模型借用
// （glm-5.2 签发的 id 送给 deepseek-v4.1-flash 会被 400）。
func scopeOf(account, model string) string {
	return account + "|" + model
}

func bindKey(scope, clientID string) string { return scope + "|" + clientID }

func (v *idVault) ensureLocked() {
	if v.loaded {
		return
	}
	v.loaded = true
	raw, err := os.ReadFile(v.path)
	if err != nil {
		return
	}
	var st idVaultState
	if err := json.Unmarshal(raw, &st); err != nil {
		logf("idvault: 忽略损坏的台账 %s: %v", v.path, err)
		return
	}
	if st.Pools == nil {
		st.Pools = map[string][]idRecord{}
	}
	if st.Bindings == nil {
		st.Bindings = map[string]idBinding{}
	}
	v.st = st
}

func (v *idVault) saveLocked() {
	raw, err := json.Marshal(v.st)
	if err != nil {
		return
	}
	dir := filepath.Dir(v.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp := v.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		logf("idvault: 写台账失败: %v", err)
		return
	}
	if err := os.Rename(tmp, v.path); err != nil {
		logf("idvault: 落盘台账失败: %v", err)
	}
}

// Harvest 记录上游签发的 call_id。
func (v *idVault) Harvest(scope, id string) {
	if v == nil || id == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ensureLocked()
	pool := v.st.Pools[scope]
	for _, rec := range pool {
		if rec.ID == id {
			return
		}
	}
	pool = append(pool, idRecord{ID: id, At: time.Now().Unix()})
	if len(pool) > idVaultPoolCap {
		pool = pool[len(pool)-idVaultPoolCap:]
	}
	v.st.Pools[scope] = pool
	v.saveLocked()
}

// Assign 给客户端的 call_id 分配真 id，尽量一对一、不重复。
// 返回已分配映射，以及仍然缺货的客户端 id。
func (v *idVault) Assign(scope string, clients []string) (map[string]string, []string) {
	out := map[string]string{}
	if v == nil || len(clients) == 0 {
		return out, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ensureLocked()

	now := time.Now().Unix()
	used := map[string]bool{}
	for _, b := range v.st.Bindings {
		if now-b.At < int64(idVaultTTL.Seconds()) {
			used[b.Real] = true
		}
	}
	var free []string
	for _, rec := range v.st.Pools[scope] {
		if now-rec.At >= int64(idVaultTTL.Seconds()) {
			continue
		}
		if !used[rec.ID] {
			free = append(free, rec.ID)
		}
	}

	var missing []string
	changed := false
	for _, client := range clients {
		if client == "" {
			continue
		}
		if _, ok := out[client]; ok {
			continue
		}
		key := bindKey(scope, client)
		if b, ok := v.st.Bindings[key]; ok && now-b.At < int64(idVaultTTL.Seconds()) {
			out[client] = b.Real
			continue
		}
		if len(free) > 0 {
			real := free[0]
			free = free[1:]
			v.st.Bindings[key] = idBinding{Real: real, At: now}
			out[client] = real
			changed = true
			continue
		}
		missing = append(missing, client)
	}
	if changed {
		v.saveLocked()
	}
	return out, missing
}

// Backfill 缺货兜底：把剩下的客户端 id 绑到最近一个真 id 上。
// 复用同一个真 id 上游是允许的（实测同请求里两条同 id 也是 200），
// 只是「真实性」打折，好过整轮失败。
func (v *idVault) Backfill(scope string, missing []string, mapping map[string]string) map[string]string {
	if v == nil || len(missing) == 0 {
		return mapping
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ensureLocked()
	latest := ""
	pool := v.st.Pools[scope]
	for i := len(pool) - 1; i >= 0; i-- {
		if time.Since(time.Unix(pool[i].At, 0)) < idVaultTTL {
			latest = pool[i].ID
			break
		}
	}
	if latest == "" {
		return mapping
	}
	if mapping == nil {
		mapping = map[string]string{}
	}
	now := time.Now().Unix()
	for _, client := range missing {
		if client == "" {
			continue
		}
		key := bindKey(scope, client)
		v.st.Bindings[key] = idBinding{Real: latest, At: now}
		mapping[client] = latest
	}
	v.saveLocked()
	return mapping
}

// PoolSize 供日志/自检查看某个分区攒了多少真 id。
func (v *idVault) PoolSize(scope string) int {
	if v == nil {
		return 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ensureLocked()
	return len(v.st.Pools[scope])
}

// collectToolCallIDs 按出现顺序收集请求里所有客户端工具调用 id。
func collectToolCallIDs(body map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	push := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, raw := range anySlice(body["messages"]) {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch m["role"] {
		case "tool":
			if id, ok := m["tool_call_id"].(string); ok {
				push(id)
			}
		case "assistant":
			for _, rc := range anySlice(m["tool_calls"]) {
				tc, ok := rc.(map[string]any)
				if !ok {
					continue
				}
				if id, ok := tc["id"].(string); ok {
					push(id)
				}
			}
		}
	}
	return out
}

// dumpFailedPayload 把上游 400 时的**转换后请求体**落到 dump 目录，用于事后定位。
// 代理层的 capture 只留 requestBody 的压缩预览（体积对不上原件），
// 复现一次 400 需要真 payload，所以这里单独存一份，只保留最近 20 份。
func dumpFailedPayload(scope, model, detail string, payload map[string]any) {
	v := getIDVault()
	if v == nil {
		return
	}
	dir := filepath.Join(filepath.Dir(v.path), "dump")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	rec := map[string]any{
		"time":    time.Now().Format(time.RFC3339),
		"scope":   scope,
		"model":   model,
		"detail":  detail,
		"payload": payload,
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	name := fmt.Sprintf("%d-%s.json", time.Now().UnixNano(), sanitizeFilePart(model))
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		return
	}
	if entries, derr := os.ReadDir(dir); derr == nil && len(entries) > 20 {
		for i := 0; i < len(entries)-20; i++ {
			_ = os.Remove(filepath.Join(dir, entries[i].Name()))
		}
	}
	logf("[%s] 上游 400，已存请求体 → %s/%s", scope, dir, name)
}

func sanitizeFilePart(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '/', '\\', ' ', ':':
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// Invalidate 把一批真 id 从池里剔除：上游按 id 查库，池里可能混进已失效的
// （实测同批里两个 38 分钟前签发的 id 单独使用就 400，其余同批次正常）。
// 剔除后粘性绑定仍指向它们也没关系 —— 失败重试路径会整体换新 id。
func (v *idVault) Invalidate(scope string, ids []string) {
	if v == nil || len(ids) == 0 {
		return
	}
	dead := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			dead[id] = true
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ensureLocked()
	pool := v.st.Pools[scope]
	kept := pool[:0]
	removed := 0
	for _, rec := range pool {
		if dead[rec.ID] {
			removed++
			continue
		}
		kept = append(kept, rec)
	}
	if removed == 0 {
		return
	}
	v.st.Pools[scope] = kept
	v.saveLocked()
	logf("[%s] 台账剔除 %d 个失效 id", scope, removed)
}
