package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrAuthExpired 登录凭据失效（401/403），需要重新登录 AStudio 客户端。
var ErrAuthExpired = fmt.Errorf("登录已失效")

// authCodes：讯飞把「登录态失效」放在响应体的 code 里，HTTP 状态仍是 200
// （body 形如 {"flag":false,"code":80000,"desc":"登录异常，请重新登录"}）。
// 实测：ssoSessionId 失效时返回 code=80000。
//
// 这类错误必须包装成 ErrAuthExpired。否则槽位不进冷却，/health 会继续
// 把死账号报成健康，池子还会往它派单 —— 静默失败，且报出来的健康数是假的。
var authCodes = map[string]bool{"80000": true}

func authCode(code, desc string) bool {
	if authCodes[code] {
		return true
	}
	return strings.Contains(desc, "重新登录") || strings.Contains(desc, "登录异常")
}

// Upstream 一个账号的上游视图。
type Upstream struct {
	account *Account
	client  *http.Client

	mu             sync.RWMutex
	models         []Model
	modelsSyncedAt float64
	lastErr        string
}

func newUpstream(a *Account, c *http.Client) *Upstream {
	// 预置全量已知目录（而非对外标准集）：这里管「认不认识这个模型名」，
	// 对外暴露的收窄由 Gateway.unionModels 单独负责。
	return &Upstream{account: a, client: c, models: aliasModels()}
}

func (u *Upstream) Models() []Model {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := make([]Model, len(u.models))
	copy(out, u.models)
	return out
}

func (u *Upstream) ModelsSynced() bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.modelsSyncedAt > 0
}

func (u *Upstream) ModelByID(mid string) *Model {
	low := strings.ToLower(mid)
	u.mu.RLock()
	defer u.mu.RUnlock()
	for i := range u.models {
		if strings.ToLower(u.models[i].ID) == low ||
			strings.ToLower(u.models[i].Name) == low {
			m := u.models[i]
			return &m
		}
	}
	return nil
}

// Resolve 把用户传的 model 解析成含 base_url 的条目；未知则透传给默认上游。
func (u *Upstream) Resolve(mid string) Model {
	if hit := u.ModelByID(mid); hit != nil {
		return *hit
	}
	return Model{ID: mid, Name: mid, BaseURL: DefaultMaasBase}
}

// Supports 该账号的服务端目录里有没有这个模型。
// 目录未同步过（只有兜底表）时不阻拦 —— 交给上游判。
func (u *Upstream) Supports(mid string) bool {
	if !u.ModelsSynced() {
		return true
	}
	return u.ModelByID(mid) != nil
}

// ---------------------------------------------------------------------------
// 基础请求
// ---------------------------------------------------------------------------

func (u *Upstream) studioGet(ctx context.Context, path string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, StudioBase+path, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range u.account.StudioHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Del("Content-Type")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("%w (HTTP %d)", ErrAuthExpired, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("响应不是 JSON: %w", err)
	}
	if flag, ok := out["flag"].(bool); ok && !flag {
		if code, ok := out["code"]; ok && fmt.Sprint(code) != "0" {
			// 登录态失效在这里被识别（不是 401/403），见 authCodes 注释。
			if authCode(fmt.Sprint(code), fmt.Sprint(out["desc"])) {
				return nil, fmt.Errorf("%w (code=%v %v)", ErrAuthExpired, code, out["desc"])
			}
			return nil, fmt.Errorf("%v (code=%v)", out["desc"], code)
		}
	}
	return out, nil
}

func (u *Upstream) studioPost(ctx context.Context, path string, body any) (map[string]any, error) {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		buf = bytes.NewReader(b)
	} else {
		buf = bytes.NewReader([]byte("{}"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, StudioBase+path, buf)
	if err != nil {
		return nil, err
	}
	for k, v := range u.account.StudioHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("%w (HTTP %d)", ErrAuthExpired, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("响应不是 JSON: %w", err)
	}
	// 与 studioGet 同理：登录态失效可能藏在 body 的 code 里（HTTP 200）。
	// 只拦鉴权码，其余业务码照旧交给调用方读 r["code"]（如签到/领奖回执）。
	if flag, ok := out["flag"].(bool); ok && !flag {
		if code, ok := out["code"]; ok && fmt.Sprint(code) != "0" {
			if authCode(fmt.Sprint(code), fmt.Sprint(out["desc"])) {
				return nil, fmt.Errorf("%w (code=%v %v)", ErrAuthExpired, code, out["desc"])
			}
		}
	}
	return out, nil
}

func dataOf(j map[string]any) map[string]any {
	if d, ok := j["data"].(map[string]any); ok {
		return d
	}
	return map[string]any{}
}

// ---------------------------------------------------------------------------
// 模型目录
// ---------------------------------------------------------------------------

// SyncModels 拉服务端模型清单（同时验证 cookie），并取回各模型 api_key。
func (u *Upstream) SyncModels(ctx context.Context) ([]Model, error) {
	j, err := u.studioGet(ctx, ModelsConfigPath)
	if err != nil {
		return nil, err
	}
	if code, ok := j["code"]; ok && fmt.Sprint(code) != "0" && fmt.Sprint(code) != "<nil>" {
		return nil, fmt.Errorf("上游返回 code=%v desc=%v", code, j["desc"])
	}
	items, _ := j["data"].([]any)
	var out []Model
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		mid := strings.TrimSpace(fmt.Sprint(m["model"]))
		if mid == "" || mid == "<nil>" {
			continue
		}
		name := strings.TrimSpace(fmt.Sprint(m["name"]))
		if name == "" || name == "<nil>" {
			name = mid
		}
		base := strings.TrimSpace(fmt.Sprint(m["base_url"]))
		if base == "" || base == "<nil>" {
			base = DefaultMaasBase
		}
		key := strings.TrimSpace(fmt.Sprint(m["api_key"]))
		if key == "<nil>" {
			key = ""
		}
		isDef, _ := m["is_default"].(bool)
		isCur, _ := m["is_current"].(bool)
		out = append(out, Model{
			ID: mid, Name: name, Multiplier: m["point_multiplier"],
			BaseURL: strings.TrimRight(base, "/"), Provider: m["provider"],
			APIKey: key, IsDefault: isDef, IsCurrent: isCur,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("上游模型清单为空")
	}
	known := map[string]bool{}
	for _, m := range out {
		known[m.ID] = true
	}
	// 服务端清单可能不含我们已知的条目（换版/灰度），补齐已知目录，
	// 让 canonicalModel 仍然认得全部模型名。对外暴露由 unionModels 收窄。
	for _, m := range aliasModels() {
		if !known[m.ID] {
			out = append(out, m)
		}
	}

	u.mu.Lock()
	u.models = out
	u.modelsSyncedAt = float64(time.Now().Unix())
	u.lastErr = ""
	u.mu.Unlock()

	// 服务端下发的 api_key 若与本地不同 → 轮换了，立刻换用新的
	fresh := ""
	for _, m := range out {
		if m.IsCurrent && m.APIKey != "" {
			fresh = m.APIKey
			break
		}
	}
	if fresh == "" {
		for _, m := range out {
			if m.APIKey != "" {
				fresh = m.APIKey
				break
			}
		}
	}
	if fresh != "" && fresh != u.account.Bearer() {
		u.account.setStr("modelBearerToken", fresh)
		if err := u.account.Save(); err != nil {
			logf("[%s] api_key 写回失败(内存生效): %v", u.account.Name, err)
		} else {
			logf("[%s] 检测到 api_key 轮换，已写回账号文件", u.account.Name)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 积分
// ---------------------------------------------------------------------------

func (u *Upstream) PointsBalance(ctx context.Context) (map[string]any, error) {
	j, err := u.studioGet(ctx, "points/balance")
	if err != nil {
		return nil, err
	}
	return dataOf(j), nil
}

func (u *Upstream) PointsDetails(ctx context.Context, page, size int) (map[string]any, error) {
	j, err := u.studioGet(ctx, fmt.Sprintf("points/details?pageNum=%d&pageSize=%d", page, size))
	if err != nil {
		return nil, err
	}
	return dataOf(j), nil
}

// collectCredits 把 points/balance 的扁平字段整理成分段结构（供池子排序）。
func collectCredits(b map[string]any) (any, []Segment) {
	if len(b) == 0 {
		return nil, nil
	}
	var segs []Segment
	for _, prefix := range []string{"member", "buy", "activity", "spark", "gift", "bonus"} {
		total, hasT := b[prefix+"Total"]
		remain, hasR := b[prefix+"Balance"]
		if !hasT && !hasR {
			continue
		}
		segs = append(segs, Segment{
			Source: prefix, Total: total, Remaining: remain,
			ExpiresAt: parseTS(b[prefix+"NextExpireTime"]),
		})
	}
	total := b["totalBalance"]
	if total == nil {
		total = b["totalAmount"]
	}
	if len(segs) == 0 {
		segs = append(segs, Segment{Source: "total", Total: b["totalAmount"], Remaining: b["totalBalance"]})
	}
	return total, segs
}

func (u *Upstream) FetchCredits(ctx context.Context) (any, []Segment, error) {
	b, err := u.PointsBalance(ctx)
	if err != nil {
		return nil, nil, err
	}
	total, segs := collectCredits(b)
	return total, segs, nil
}

// ---------------------------------------------------------------------------
// 签到 / 领奖
// ---------------------------------------------------------------------------

func (u *Upstream) pendingPopups(ctx context.Context) ([]map[string]any, error) {
	j, err := u.studioGet(ctx, "client-popups/pending")
	if err != nil {
		return nil, err
	}
	items, _ := j["data"].([]any)
	var out []map[string]any
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// CheckinResult 一轮领奖的战果。
type CheckinResult struct {
	OK      bool             `json:"ok"`
	Popups  []map[string]any `json:"popups,omitempty"`
	Claimed []map[string]any `json:"claimed"`
	Errors  []string         `json:"errors"`
	Before  *float64         `json:"balance_before,omitempty"`
	After   *float64         `json:"balance_after,omitempty"`
	Gained  float64          `json:"gained"`
	// 星火积分（活动期加成，如国庆 5000）单独计量——它不在 totalAmount 里，
	// 只读通用积分会让签到战果看起来像 0。
	SparkBefore *float64 `json:"spark_before,omitempty"`
	SparkAfter  *float64 `json:"spark_after,omitempty"`
	SparkGained float64  `json:"spark_gained"`
	// 当前可用余额（会随消耗下降），与累计发放的 *_after 区分开
	Balance      *float64 `json:"balance,omitempty"`
	SparkBalance *float64 `json:"spark_balance,omitempty"`
	Banned       bool     `json:"banned,omitempty"`
	Dry          bool     `json:"dry,omitempty"`
}

// points 一次读全通用/星火两本账。
//
// 计量口径（踩过坑）：totalAmount 是「累计发放」，单调递增，必须用它算签到战果；
// totalBalance 是「当前可用」，会被模型调用消耗掉。早期代码优先读 totalBalance，
// 结果同一天连续签到/用过分之后再签到，战果会被算成负数或 0。
type points struct {
	granted      *float64 // totalAmount      累计发放
	available    *float64 // totalBalance     当前可用
	sparkGranted *float64 // sparkTotalAmount 星火累计发放
	sparkAvail   *float64 // sparkTotalBalance 星火当前可用
}

func (u *Upstream) readPoints(ctx context.Context) (points, error) {
	b, err := u.PointsBalance(ctx)
	if err != nil {
		return points{}, err
	}
	return points{
		granted:      num(b["totalAmount"]),
		available:    num(b["totalBalance"]),
		sparkGranted: num(b["sparkTotalAmount"]),
		sparkAvail:   num(b["sparkTotalBalance"]),
	}, nil
}

// boolOf 取 init-app 返回里的 data.banned。
func boolOf(r map[string]any) bool {
	if d, ok := r["data"].(map[string]any); ok {
		if b, ok := d["banned"].(bool); ok {
			return b
		}
	}
	return false
}

func num(v any) *float64 {
	if f, ok := toFloat(v); ok {
		return &f
	}
	return nil
}

// SignIn 执行一次「登录签到」。
//
// 这是账号真正拿到每日积分的那一步：客户端启动登录时调的
// POST tenant-app/v2/init-app。它一次发两笔，按天幂等（同一天重复调只发一次）：
//   - 常规每日登录积分（体验版 100，标准版 200，高级/畅享 400）
//   - 活动期加成，例如 2026 国庆（10-01~10-07）额外 5000 星火积分
//
// 注意：client-popups/pending 里那些弹窗（CLIENT_DOWNLOAD_REWARD_DIALOG /
// DAILY_REWARD_DIALOG）都只是 UI 提示，对应的 claim / complete 都是空操作
// —— 实测连已领过的账号调 claim 也返回成功且余额不变。真正发分的就是这里。
func (u *Upstream) SignIn(ctx context.Context) (map[string]any, error) {
	r, err := u.studioPost(ctx, "tenant-app/v2/init-app", nil)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (u *Upstream) CheckinOnce(ctx context.Context, dry bool) (*CheckinResult, error) {
	res := &CheckinResult{OK: true, Claimed: []map[string]any{}, Errors: []string{}, Dry: dry}

	readInto := func(p points) {
		res.Before, res.Balance = p.granted, p.available
		res.SparkBefore, res.SparkBalance = p.sparkGranted, p.sparkAvail
	}
	if p, err := u.readPoints(ctx); err == nil {
		readInto(p)
	} else if isAuthErr(err) {
		return nil, err
	} else {
		res.Errors = append(res.Errors, "读余额失败: "+err.Error())
	}

	// ① 真正的签到：init-app。必须在处理弹窗之前做——发分的是它。
	if !dry {
		r, err := u.SignIn(ctx)
		if err != nil {
			if isAuthErr(err) {
				return nil, err
			}
			res.Errors = append(res.Errors, "签到(init-app)失败: "+err.Error())
		} else {
			res.Banned = boolOf(r)
			res.Claimed = append(res.Claimed, map[string]any{
				"type": "SIGN_IN", "code": r["code"], "desc": r["desc"],
				"banned": res.Banned,
			})
			if r["code"] != nil && fmt.Sprint(r["code"]) != "0" {
				res.Errors = append(res.Errors,
					fmt.Sprintf("签到返回 code=%v desc=%v", r["code"], r["desc"]))
			} else if res.Banned {
				res.Errors = append(res.Errors, "账号已被封禁（banned=true）")
			}
		}
	}

	popups, err := u.pendingPopups(ctx)
	if err != nil {
		if isAuthErr(err) {
			return nil, err
		}
		res.Errors = append(res.Errors, "拉待领弹窗失败: "+err.Error())
		res.OK = false
		return res, nil
	}
	res.Popups = popups
	if !dry && len(popups) > 0 {
		for _, p := range popups {
			item, err := u.claimPopup(ctx, p)
			if err != nil {
				if isAuthErr(err) {
					return nil, err
				}
				res.Errors = append(res.Errors, fmt.Sprintf("领取弹窗 %v 失败: %v", p["popupId"], err))
				continue
			}
			if item != nil {
				res.Claimed = append(res.Claimed, item)
			}
		}
	}

	// 收尾：再读一次，用「累计发放」的差算战果（不会被消耗干扰）
	if p, err := u.readPoints(ctx); err == nil {
		res.After, res.Balance = p.granted, p.available
		res.SparkAfter, res.SparkBalance = p.sparkGranted, p.sparkAvail
		if res.Before != nil && res.After != nil {
			res.Gained = *res.After - *res.Before
		}
		if res.SparkBefore != nil && res.SparkAfter != nil {
			res.SparkGained = *res.SparkAfter - *res.SparkBefore
		}
	}
	res.OK = len(res.Errors) == 0
	return res, nil
}

// claimPopup 处理单个待领弹窗。
//
// 实测提醒：这些弹窗接口在服务端都是空操作——client-download-reward/claim
// 连早已领过的账号也返回 flag:true/code:0 且余额不变，client-popups/complete
// 只是把弹窗标记成「已读」。所以它们只负责打扫 UI 状态，别指望它们发积分；
// 积分由上面的 init-app 发。留着这段是为了让客户端侧的状态跟真实登录一致。
func (u *Upstream) claimPopup(ctx context.Context, p map[string]any) (map[string]any, error) {
	pidF, ok := toFloat(p["popupId"])
	ikey := strings.TrimSpace(fmt.Sprint(p["instanceKey"]))
	ctype := fmt.Sprint(p["componentType"])
	if !ok || ikey == "" || ikey == "<nil>" {
		return nil, fmt.Errorf("条目不完整: popupId=%v instanceKey=%v", p["popupId"], p["instanceKey"])
	}
	pid := int(pidF)

	var (
		r   map[string]any
		err error
	)
	if ctype == "CLIENT_DOWNLOAD_REWARD_DIALOG" {
		r, err = u.studioPost(ctx, "client-download-reward/claim", nil)
	} else {
		r, err = u.studioPost(ctx, "client-popups/complete",
			map[string]any{"popupId": pid, "instanceKey": ikey})
	}
	if err != nil {
		return nil, err
	}
	code := r["code"]
	item := map[string]any{"type": ctype, "popupId": pid, "code": code, "desc": r["desc"]}
	if code != nil && fmt.Sprint(code) != "0" {
		return item, fmt.Errorf("%s 返回 code=%v desc=%v", ctype, code, r["desc"])
	}
	return item, nil
}

// ---------------------------------------------------------------------------
// 推理
// ---------------------------------------------------------------------------

func (u *Upstream) target(body map[string]any, suffix string) (string, string) {
	mid := strings.TrimSpace(fmt.Sprint(body["model"]))
	entry := u.Resolve(mid)
	key := entry.APIKey
	if key == "" {
		key = u.account.Bearer()
	}
	return strings.TrimRight(entry.BaseURL, "/") + suffix, key
}

// Chat 发起对话请求；调用方负责关闭 resp.Body。
func (u *Upstream) Chat(ctx context.Context, body map[string]any) (*http.Response, error) {
	url, key := u.target(body, "/chat/completions")
	return u.doJSON(ctx, url, key, body)
}

// Responses 发起 responses 请求。
func (u *Upstream) Responses(ctx context.Context, body map[string]any) (*http.Response, error) {
	url, key := u.target(body, "/responses")
	return u.doJSON(ctx, url, key, body)
}

func (u *Upstream) doJSON(ctx context.Context, url, key string, body map[string]any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	for k, v := range u.account.MaasHeaders(key) {
		req.Header.Set(k, v)
	}
	return u.client.Do(req)
}

// errFromResponse 把非 2xx 响应转成错误（读掉 body 后关闭）。
func errFromResponse(resp *http.Response) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200))
}

var _ = url.Values{}
var _ = strconv.Itoa
