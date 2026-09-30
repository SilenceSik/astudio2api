package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 共享 HTTP 客户端
// ---------------------------------------------------------------------------

type httpClient struct {
	HTTP *http.Client
}

func newHTTPClient() *httpClient {
	return &httpClient{HTTP: &http.Client{
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			MaxIdleConns:        128,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
		// 不设总超时：流式请求可能很长；用请求级 context 控制
	}}
}

// ---------------------------------------------------------------------------
// 网关
// ---------------------------------------------------------------------------

type Gateway struct {
	cfg  Config
	pool *Pool
}

func (g *Gateway) auth(w http.ResponseWriter, r *http.Request) bool {
	if g.cfg.APIKey == "" {
		return true
	}
	if strings.TrimSpace(r.Header.Get("Authorization")) != "Bearer "+g.cfg.APIKey {
		writeErr(w, http.StatusUnauthorized, "Invalid API key")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "astudio2api_error"}})
}

func writeUpstreamErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "upstream_error"}})
}

// sessionKey 会话键：优先客户端显式头，其次 body.user。
func sessionKey(r *http.Request, body map[string]any) string {
	for _, h := range []string{"x-hermes-session-id", "x-session-id", "x-conversation-id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	if body != nil {
		if u, ok := body["user"].(string); ok && strings.TrimSpace(u) != "" {
			return "user:" + strings.TrimSpace(u)
		}
	}
	return ""
}

func readBody(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("读取请求体失败: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("请求体为空")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", g.handleHealth)
	mux.HandleFunc("/v1/models", g.handleModels)
	mux.HandleFunc("/v1/chat/completions", g.handleChat)
	mux.HandleFunc("/v1/responses", g.handleResponses)
	mux.HandleFunc("/admin/accounts", g.handleAccounts)
	mux.HandleFunc("/admin/accounts/import", g.handleImport)
	mux.HandleFunc("/admin/accounts/login", g.handleAccountLogin)
	mux.HandleFunc("/admin/accounts/", g.handleAccountByName)
	mux.HandleFunc("/admin/reload", g.handleReload)
	mux.HandleFunc("/admin/sync", g.handleSync)
	mux.HandleFunc("/admin/checkin", g.handleCheckin)
	mux.HandleFunc("/admin/credits", g.handleCredits)
	mux.HandleFunc("/admin/credits/refresh", g.handleCreditsRefresh)
	mux.HandleFunc("/admin/points", g.handlePoints)
	mux.HandleFunc("/admin/models", g.handleAdminModels)

	return mux
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	slots := g.pool.Slots()
	healthy := 0
	var dead []string
	for _, s := range slots {
		if s.Healthy() {
			healthy++
		} else if s.CredDead() {
			// 凭据失效的账号单独报出来，不混进 healthy 数里。
			// 之前这里只数 failUntil，死账号在冷却过期后被算成健康。
			dead = append(dead, s.Name())
		}
	}
	st := "ok"
	if len(slots) == 0 || healthy == 0 {
		st = "degraded"
	} else if len(dead) > 0 {
		st = "degraded"
	}
	out := map[string]any{
		"status": st, "accounts": len(slots), "healthy": healthy,
		"models": len(g.unionModels()),
	}
	if len(dead) > 0 {
		out["needs_relogin"] = dead
	}
	writeJSON(w, 200, out)
}

// unionModels 对外暴露的模型清单：以标准集为骨架，再用各账号上游同步
// 回来的真实数据（倍率、显示名）补齐。
//
// 之所以不直接取全账号并集：受限模型只存在于老号，并集会把它暴露给所有人，
// 请求落到没有订购的账号上就是 403，还白烧一次该模型的冷却。
func (g *Gateway) unionModels() []Model {
	standard := standardModelIDs()
	byID := map[string]Model{}
	for _, m := range builtinModels() {
		byID[m.ID] = m
	}
	for _, s := range g.pool.Slots() {
		for _, m := range s.Up.Models() {
			cur, ok := byID[m.ID]
			if !ok || !standard[m.ID] {
				continue
			}
			if m.Multiplier != nil {
				cur.Multiplier = m.Multiplier
			}
			if m.Name != "" {
				cur.Name = m.Name
			}
			byID[m.ID] = cur
		}
	}
	out := make([]Model, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// normalizeModelKey 把模型名归一成比对键：小写、去空格与下划线、`.`/`-` 统一。
// 调用方写 `GLM-5.2`、`glm-5.2`、`glm 5.2` 都应命中同一个模型。
func normalizeModelKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ' || r == '/':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// compactModelKey 比 normalizeModelKey 更宽松：连分隔符一起丢掉。
// `GLM_5_2` → `glm52`，`glm-5.2` → `glm52`，`glm.5.2` → `glm52`。
// 两道键互相补充：前者管大小写与空格，后者管下划线/连字符/点号混用
// ——人工手输模型名时这几种写法都会被用上，而它们指的是同一个模型。
func compactModelKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func modelKeyMatches(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || compactModelKey(a) == compactModelKey(b)
}

// canonicalModel 把客户端传来的模型名归一成**上游 id**。
//
// 接受三种写法，全部指向同一个模型：
//   - 友好别名：`glm-5.2`（对外暴露、推荐）
//   - 上游显示名：`GLM-5.2`
//   - 上游内部代号：`xopglm52`（兼容既有渠道配置，不破坏存量）
//
// 上游只认内部代号，所以命中后把 body 里的 model 改写成代号再发出去。
// 都没命中就原样透传，交给上游判（让它回 404 比我们猜更准）。
func (g *Gateway) canonicalModel(mid string) string {
	key := normalizeModelKey(mid)
	if key == "" {
		return mid
	}
	try := func(list []Model) (string, bool) {
		for _, m := range list {
			for _, cand := range []string{m.ID, m.Alias(), m.Name} {
				if modelKeyMatches(normalizeModelKey(cand), key) {
					return m.ID, true
				}
			}
		}
		return "", false
	}
	if id, ok := try(g.unionModels()); ok {
		return id
	}
	// 对外清单只含标准集，但解析要覆盖全量已知目录 —— 否则受限模型的
	// 友好名（GLM-5.1 等）会解析不出 slug，原样透传给上游吃 404。
	if id, ok := try(aliasModels()); ok {
		return id
	}
	return mid
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	now := time.Now().Unix()
	var data []map[string]any
	for _, m := range g.unionModels() {
		data = append(data, map[string]any{
			"id": m.Alias(), "object": "model", "created": now, "owned_by": "xfyun",
			"root": m.ID, "multiplier": m.Multiplier,
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{
		"count": len(g.pool.Slots()), "accounts": g.accountSummaries(),
		"load_errors": g.pool.LoadErrors(),
	})
}

func (g *Gateway) accountSummaries() []map[string]any {
	out := []map[string]any{}
	for _, s := range g.pool.Slots() {
		cd, lastErr := s.Status()
		var credits, expiry any
		segCount := 0
		if c := g.pool.ledger.CreditsOf(s.Identity()); c != nil {
			credits = c.Total
			segCount = len(c.Segments)
			if c.SoonestExpiry > 0 {
				expiry = c.SoonestExpiry
			}
		}
		var ck any
		if c := g.pool.ledger.CheckinOf(s.Identity()); c != nil {
			ck = c
		}
		out = append(out, map[string]any{
			"name": s.Name(), "identity": s.Identity(), "uid": s.Account.UID(),
			"models": len(s.Up.Models()), "models_synced": s.Up.ModelsSynced(),
			"healthy": s.Healthy(), "cooldown_s": round1(cd), "last_error": lastErr,
			"credits": credits, "segments": segCount, "soonest_expiry": expiry,
			"checkin": ck, "banned": s.Account.Banned(),
		})
	}
	return out
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

func (g *Gateway) handleAccountLogin(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	var body struct {
		Account   string `json:"account"`
		Password  string `json:"password"`
		Name      string `json:"name"`
		Overwrite *bool  `json:"overwrite"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是 JSON")
		return
	}
	overwrite := true
	if body.Overwrite != nil {
		overwrite = *body.Overwrite
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	a, err := LoginAndAddAccount(ctx, g.cfg, g.pool.client.HTTP,
		body.Account, body.Password, strings.TrimSpace(body.Name), overwrite)
	if err != nil {
		// 密码不落任何日志；错误里也只带账号名。
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "已存在") {
			code = http.StatusConflict
		}
		writeErr(w, code, err.Error())
		return
	}
	g.pool.Reload()
	logf("[%s] 通过账号密码登录新增（accountId=%s）", a.Name, mask(a.AccountID()))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "account": a.Summary(),
		"pool": map[string]any{"accounts": len(g.pool.Slots())},
	})
}

func (g *Gateway) handleImport(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	src := strings.TrimSpace(q.Get("path"))
	overwrite := q.Get("overwrite") != "false"
	var (
		a   *Account
		err error
	)
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	if src != "" {
		a, err = addAccountFile(g.cfg, src, name, overwrite)
	} else {
		a, err = importLiveSession(g.cfg, name, overwrite)
	}
	if err != nil {
		code := http.StatusInternalServerError
		if strings.Contains(err.Error(), "已存在") {
			code = http.StatusConflict
		} else if strings.Contains(err.Error(), "找不到") || strings.Contains(err.Error(), "不存在") {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	g.pool.Reload()
	if s := g.pool.Find(a.Name); s != nil {
		if _, e := g.pool.RefreshModels(ctx, s); e != nil {
			logf("导入后同步失败: %v", e)
		}
		if e := g.pool.RefreshCredits(ctx, s); e != nil {
			logf("导入后同步失败: %v", e)
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "account": a.Summary(), "accounts": len(g.pool.Slots())})
}

func (g *Gateway) handleAccountByName(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 DELETE")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/admin/accounts/")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "缺少账号名")
		return
	}
	if s := g.pool.Find(name); s != nil {
		g.pool.ledger.Remove(s.Identity())
	}
	if !removeAccount(g.cfg, name) {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name)
		return
	}
	g.pool.Reload()
	writeJSON(w, 200, map[string]any{
		"ok": true, "removed": name, "accounts": len(g.pool.Slots())})
}

func (g *Gateway) handleReload(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	g.pool.Reload()
	writeJSON(w, 200, map[string]any{
		"ok": true, "accounts": len(g.pool.Slots()), "load_errors": g.pool.LoadErrors()})
}

func (g *Gateway) handleSync(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 240*time.Second)
	defer cancel()
	writeJSON(w, 200, g.pool.SyncAll(ctx, true, true, 4))
}

func (g *Gateway) handleCheckin(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	q := r.URL.Query()
	dry := q.Get("dry") == "true"
	force := q.Get("force") == "true"
	ctx, cancel := context.WithTimeout(r.Context(), 240*time.Second)
	defer cancel()
	writeJSON(w, 200, map[string]any{
		"ok": true, "results": g.pool.CheckinAll(ctx, dry, force, 2)})
}

func (g *Gateway) creditsPayload() map[string]any {
	accounts := []map[string]any{}
	total := 0.0
	for _, s := range g.pool.Slots() {
		row := map[string]any{"name": s.Name(), "identity": s.Identity()}
		if c := g.pool.ledger.CreditsOf(s.Identity()); c != nil {
			row["total"] = c.Total
			row["segments"] = c.Segments
			if c.SoonestExpiry > 0 {
				row["soonest_expiry"] = c.SoonestExpiry
			}
			row["fetched_at"] = c.FetchedAt
			if f, ok := toFloat(c.Total); ok {
				total += f
			}
		}
		if c := g.pool.ledger.CheckinOf(s.Identity()); c != nil {
			row["checkin"] = c
		}
		if e := g.pool.ledger.ErrorOf(s.Identity()); e != "" {
			row["error"] = e
		}
		accounts = append(accounts, row)
	}
	return map[string]any{"total": round1(total), "count": len(accounts), "accounts": accounts}
}

func (g *Gateway) handleCredits(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	writeJSON(w, 200, g.creditsPayload())
}

func (g *Gateway) handleCreditsRefresh(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 240*time.Second)
	defer cancel()
	res := g.pool.SyncAll(ctx, false, true, 4)
	writeJSON(w, 200, map[string]any{
		"ok": len(res.Errors) == 0, "errors": res.Errors, "credits": g.creditsPayload()})
}

func (g *Gateway) handlePoints(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("account"))
	var s *Slot
	if name != "" {
		s = g.pool.Find(name)
	} else if all := g.pool.Slots(); len(all) > 0 {
		s = all[0]
	}
	if s == nil {
		writeErr(w, http.StatusNotFound, "没有可用账号")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	bal, err := s.Up.PointsBalance(ctx)
	if err != nil {
		if isAuthErr(err) {
			writeErr(w, http.StatusUnauthorized,
				fmt.Sprintf("[%s] 登录失效，请在 AStudio 客户端重新登录", s.Name()))
			return
		}
		writeUpstreamErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"account": s.Name(), "balance": bal})
}

func (g *Gateway) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	providers := map[string][]string{}
	type row struct {
		Account    string `json:"account"`
		ID         string `json:"id"`
		Name       string `json:"name"`
		Multiplier any    `json:"multiplier"`
	}
	var rows []row
	for _, s := range g.pool.Slots() {
		for _, m := range s.Up.Models() {
			rows = append(rows, row{s.Name(), m.ID, m.Name, m.Multiplier})
			providers[m.ID] = append(providers[m.ID], s.Name())
		}
	}
	ids := make([]string, 0, len(providers))
	for k := range providers {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	writeJSON(w, 200, map[string]any{"models": ids, "providers": providers, "rows": rows})
}

// ---------------------------------------------------------------------------
// 推理
// ---------------------------------------------------------------------------

// 需要换号重试的状态：登录失效 / 限流 / 上游故障
func retryable(status int) bool {
	return status == 401 || status == 403 || status == 429 || status >= 500
}

func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	stream, _ := body["stream"].(bool)
	mid := strings.TrimSpace(fmt.Sprint(body["model"]))
	if mid == "" || mid == "<nil>" {
		writeErr(w, http.StatusBadRequest, "缺少 model")
		return
	}
	// 友好别名 → 上游代号，并改写请求体（上游只认代号）
	mid = g.canonicalModel(mid)
	body["model"] = mid
	skey := sessionKey(r, body)
	tried := map[string]bool{}
	lastErr := "没有可用账号"

	for i := 0; i < MaxAttempts; i++ {
		s := g.pool.PickExcluding(skey, mid, tried)
		if s == nil {
			break
		}
		tried[s.Identity()] = true
		resp, err := s.Up.Chat(r.Context(), body)
		if err != nil {
			s.Cooldown(CooldownError, err.Error())
			lastErr = err.Error()
			continue
		}
		if resp.StatusCode >= 400 {
			status := resp.StatusCode
			err := errFromResponse(resp)
			g.pool.NoteStatus(s, status, mid)
			lastErr = err.Error()
			if retryable(status) {
				continue
			}
			writeUpstreamErr(w, status, lastErr)
			return
		}
		s.NoteOK()
		if !stream {
			relayJSON(w, resp)
			return
		}
		streamCopy(w, resp)
		return
	}
	writeUpstreamErr(w, http.StatusBadGateway, "上游调用失败："+lastErr)
}

func (g *Gateway) handleResponses(w http.ResponseWriter, r *http.Request) {
	if !g.auth(w, r) {
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	stream := true
	if v, ok := body["stream"].(bool); ok {
		stream = v
	}
	mid := strings.TrimSpace(fmt.Sprint(body["model"]))
	// 友好别名 → 上游代号，并改写请求体（上游只认代号）
	mid = g.canonicalModel(mid)
	body["model"] = mid
	skey := sessionKey(r, body)
	tried := map[string]bool{}
	lastErr := "没有可用账号"

	for i := 0; i < MaxAttempts; i++ {
		s := g.pool.PickExcluding(skey, mid, tried)
		if s == nil {
			break
		}
		tried[s.Identity()] = true
		resp, err := s.Up.Responses(r.Context(), body)
		if err != nil {
			s.Cooldown(CooldownError, err.Error())
			lastErr = err.Error()
			continue
		}
		if resp.StatusCode >= 400 {
			status := resp.StatusCode
			lastErr = errFromResponse(resp).Error()
			g.pool.NoteStatus(s, status, mid)
			if retryable(status) {
				continue
			}
			writeUpstreamErr(w, status, lastErr)
			return
		}
		s.NoteOK()
		if !stream {
			relayJSON(w, resp)
			return
		}
		streamCopy(w, resp)
		return
	}
	writeUpstreamErr(w, http.StatusBadGateway, "上游调用失败："+lastErr)
}

func relayJSON(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		writeUpstreamErr(w, http.StatusBadGateway, "读取上游响应失败: "+err.Error())
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(raw)
}

func streamCopy(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/event-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// 后台巡检
// ---------------------------------------------------------------------------

func (g *Gateway) StartBackground(ctx context.Context) *sync.WaitGroup {
	var wg sync.WaitGroup
	run := func(fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctx)
		}()
	}

	run(func(ctx context.Context) {
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return
		}
		c, cancel := context.WithTimeout(ctx, 240*time.Second)
		defer cancel()
		res := g.pool.SyncAll(c, true, true, 4)
		for name, n := range res.Models {
			logf("账号 %s: %d 个模型", name, n)
		}
		for _, e := range res.Errors {
			logf("同步告警: %s", e)
		}
	})

	run(func(ctx context.Context) {
		select {
		case <-time.After(CheckinStartupDelay):
		case <-ctx.Done():
			return
		}
		for {
			c, cancel := context.WithTimeout(ctx, 240*time.Second)
			for _, r := range g.pool.CheckinAll(c, false, false, 2) {
				switch {
				case r.Skipped != "":
				case r.Error != "":
					logf("签到[%s] 失败: %s", r.Name, r.Error)
				default:
					if !signinSucceeded(&CheckinResult{Claimed: r.Detail, Errors: r.Errors}) {
						logf("签到[%s] 未确认成功（不占当日名额，下轮重试）: %v", r.Name, r.Errors)
						continue
					}
					gain := fmt.Sprintf("%+g", r.Gained)
					if r.SparkGained != 0 {
						gain += fmt.Sprintf(" 星火%+g", r.SparkGained)
					}
					left := ""
					if r.Balance != nil {
						left = fmt.Sprintf("，可用 %g", *r.Balance)
						if r.SparkBalance != nil && *r.SparkBalance != 0 {
							left += fmt.Sprintf("+星火%g", *r.SparkBalance)
						}
					}
					logf("签到[%s] 成功 %s%s", r.Name, gain, left)
				}
			}
			cancel()
			select {
			case <-time.After(CheckinInterval):
			case <-ctx.Done():
				return
			}
		}
	})

	run(func(ctx context.Context) {
		for {
			select {
			case <-time.After(ModelsTTL):
			case <-ctx.Done():
				return
			}
			c, cancel := context.WithTimeout(ctx, 240*time.Second)
			g.pool.SyncAll(c, true, true, 4)
			cancel()
		}
	})

	return &wg
}
