package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// testPool 造一个只含内存槽位的池子，不碰网络。
func testPool(t *testing.T, accts []*Account) *Pool {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{AccountsDir: filepath.Join(dir, "accounts"), StatePath: filepath.Join(dir, "state.json")}
	if err := os.MkdirAll(cfg.AccountsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, a := range accts {
		a.Path = filepath.Join(cfg.AccountsDir, a.Name+".json")
		if err := a.Save(); err != nil {
			t.Fatal(err)
		}
	}
	p := newPool(cfg, newLedger(cfg.StatePath))
	p.Reload()
	return p
}

func acct(name, uid, mobile string) *Account {
	return &Account{Name: name, Raw: map[string]any{
		"accountId": "acc-" + name, "uid": uid, "mobile": mobile,
		"modelBearerToken": "bearer-" + name, "ssoSessionId": "sso-" + name,
	}}
}

// seed 直接把台账与模型写进内存，绕开网络。
func seed(p *Pool, s *Slot, total any, segs []Segment, models []Model) {
	p.ledger.UpdateCredits(s.Identity(), total, segs)
	p.ledger.PutModels(s.Identity(), models)
}

func mx(v any) Model {
	return Model{ID: "m", Multiplier: v, BaseURL: DefaultMaasBase}
}

// ---------------------------------------------------------------------------
// 死锁回归 —— Pick 持锁时再调 Slots 曾导致整池永久阻塞
// ---------------------------------------------------------------------------

func TestConcurrentPickAndSlotsNoDeadlock(t *testing.T) {
	p := testPool(t, []*Account{acct("a", "u1", "13800138000"), acct("b", "u2", "13800138001")})
	for _, s := range p.Slots() {
		seed(p, s, 100, []Segment{{Source: "activity", Total: 100, Remaining: 100,
			ExpiresAt: float64(time.Now().Add(24 * time.Hour).Unix())}},
			[]Model{{ID: "xopglm52", Multiplier: "x2.0", BaseURL: DefaultMaasBase}})
	}

	// 同步阶段：Pick 内部若再次加锁，这里就会直接死锁
	if got := p.Pick("s1", "xopglm52"); got == nil {
		t.Fatal("Pick 返回 nil，池里应有可用账号")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = p.Pick("s1", "xopglm52")
			_ = p.Slots()
			_ = p.candidates("xopglm52")
			_ = p.Find("a")
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("检测到死锁：Pick/Slots/candidates 并发调用超时未返回")
	}
}

// ---------------------------------------------------------------------------
// 调度算法
// ---------------------------------------------------------------------------

func TestFreeModelWins(t *testing.T) {
	p := testPool(t, []*Account{acct("paid", "u1", "1"), acct("free", "u2", "2")})
	paid, free := p.Find("paid"), p.Find("free")
	seed(p, paid, 9999, nil, []Model{{ID: "xopglm52", Multiplier: "x2.0", BaseURL: DefaultMaasBase}})
	seed(p, free, 1, nil, []Model{{ID: "xopglm52", Multiplier: "x0", BaseURL: DefaultMaasBase}})

	for i := 0; i < 5; i++ {
		if got := p.Pick("", "xopglm52"); got == nil || got.Name() != "free" {
			t.Fatalf("第 %d 次：应优先零计费账号 free，实得 %v", i, nameOf(got))
		}
	}
}

func TestSoonestExpiryWins(t *testing.T) {
	p := testPool(t, []*Account{acct("late", "u1", "1"), acct("soon", "u2", "2")})
	now := float64(time.Now().Unix())
	seed(p, p.Find("late"), 500, []Segment{{Remaining: 500, ExpiresAt: now + 30*86400}},
		[]Model{{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase}})
	seed(p, p.Find("soon"), 500, []Segment{{Remaining: 500, ExpiresAt: now + 86400}},
		[]Model{{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase}})

	if got := p.Pick("", "m1"); got == nil || got.Name() != "soon" {
		t.Fatalf("应快过期优先 → soon，实得 %v", nameOf(got))
	}
}

func TestZeroBalanceExcludedUnlessFree(t *testing.T) {
	p := testPool(t, []*Account{acct("empty", "u1", "1")})
	s := p.Find("empty")
	seed(p, s, 0, []Segment{{Remaining: 0}}, []Model{{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase}})

	if got := p.Pick("", "m1"); got != nil {
		t.Fatalf("零余额用付费模型应被排除，实得 %v", nameOf(got))
	}
}

func TestUnknownBalanceStillEligible(t *testing.T) {
	// 还没同步过积分的账号不能被当成零余额判死，否则新导入账号无法服务
	p := testPool(t, []*Account{acct("new", "u1", "1")})
	s := p.Find("new")
	p.ledger.PutModels(s.Identity(), []Model{{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase}})

	if got := p.Pick("", "m1"); got == nil {
		t.Fatal("无积分数据（未同步）的账号应可用，不该被排除")
	}
}

func TestModelSupportFilter(t *testing.T) {
	p := testPool(t, []*Account{acct("only-glm", "u1", "1")})
	s := p.Find("only-glm")
	seed(p, s, 100, nil, []Model{{ID: "xopglm52", Multiplier: "x1.0", BaseURL: DefaultMaasBase}})

	if got := p.Pick("", "xopglm52"); got == nil {
		t.Fatal("账号支持的模型应可选")
	}
	if got := p.Pick("", "not-in-catalog"); got != nil {
		t.Fatalf("账号目录里没有的模型不应选它，实得 %v", nameOf(got))
	}
}

func TestStickyKeepsAccount(t *testing.T) {
	p := testPool(t, []*Account{acct("a", "u1", "1"), acct("b", "u2", "2")})
	for _, s := range p.Slots() {
		seed(p, s, 100, nil, []Model{{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase}})
	}
	first := p.Pick("conv-1", "m1")
	if first == nil {
		t.Fatal("应有可用账号")
	}
	for i := 0; i < 8; i++ {
		if got := p.Pick("conv-1", "m1"); got == nil || got.Identity() != first.Identity() {
			t.Fatalf("会话黏绑失效：第 %d 次选到了 %v，期望 %s",
				i, nameOf(got), first.Name())
		}
	}
	// 不同会话应能落到别的账号（同级轮询）
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		if got := p.Pick("conv-"+string(rune('a'+i)), "m1"); got != nil {
			seen[got.Identity()] = true
		}
	}
	if len(seen) < 2 {
		t.Fatalf("同级多账号应轮询，只用到 %d 个账号", len(seen))
	}
}

func TestCooldownAndRecovery(t *testing.T) {
	p := testPool(t, []*Account{acct("a", "u1", "1")})
	s := p.Find("a")
	seed(p, s, 100, nil, []Model{{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase}})

	s.Cooldown(time.Hour, "模拟 401")
	if got := p.Pick("", "m1"); got != nil {
		t.Fatalf("冷却中的账号不应被选中，实得 %v", nameOf(got))
	}
	s.NoteOK()
	if got := p.Pick("", "m1"); got == nil {
		t.Fatal("冷却解除后应可再次选中")
	}
}

func TestModelScopedCooldown(t *testing.T) {
	// 429 只该冷却出问题的那个模型，不能拖累同账号的其它模型
	p := testPool(t, []*Account{acct("a", "u1", "1")})
	s := p.Find("a")
	seed(p, s, 100, nil, []Model{
		{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase},
		{ID: "m2", Multiplier: "x1.0", BaseURL: DefaultMaasBase},
	})
	p.NoteStatus(s, 429, "m1")
	if got := p.Pick("", "m1"); got != nil {
		t.Fatal("被限流的模型应冷却")
	}
	if got := p.Pick("", "m2"); got == nil {
		t.Fatal("同账号的其它模型不该被连带冷却")
	}
}

func TestForbiddenIsModelScopedNotAccountWide(t *testing.T) {
	// 403（11200 未订购）= 账号×模型的权限属性，不是凭据失效。
	// 只该摘掉那一个模型；若按账号级冷却，会出现「一个没订购的模型
	// 把账号上其它能用的模型一起拖下水」的产能黑洞。
	p := testPool(t, []*Account{acct("a", "u1", "1")})
	s := p.Find("a")
	seed(p, s, 100, nil, []Model{
		{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase},
		{ID: "m2", Multiplier: "x1.0", BaseURL: DefaultMaasBase},
	})
	p.NoteStatus(s, 403, "m1")
	if got := p.Pick("", "m1"); got != nil {
		t.Fatal("403 的模型应冷却")
	}
	if got := p.Pick("", "m2"); got == nil {
		t.Fatal("403 只该冷却出问题的模型，同账号其它模型必须仍可用")
	}
	if !s.Healthy() {
		t.Fatal("403 不该把整个账号判为不健康（凭据依然是好的）")
	}
}

func TestUnauthorizedIsAccountWide(t *testing.T) {
	// 401 才是凭据失效——整个账号下线
	p := testPool(t, []*Account{acct("a", "u1", "1")})
	s := p.Find("a")
	seed(p, s, 100, nil, []Model{
		{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase},
		{ID: "m2", Multiplier: "x1.0", BaseURL: DefaultMaasBase},
	})
	p.NoteStatus(s, 401, "m1")
	if s.Healthy() {
		t.Fatal("401 应让整个账号下线")
	}
	if got := p.Pick("", "m2"); got != nil {
		t.Fatal("401 后同一账号的其它模型也不该再被选中")
	}
}

func TestPickExcludingAvoidsTried(t *testing.T) {
	p := testPool(t, []*Account{acct("a", "u1", "1"), acct("b", "u2", "2")})
	for _, s := range p.Slots() {
		seed(p, s, 100, nil, []Model{{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase}})
	}
	tried := map[string]bool{}
	first := p.PickExcluding("", "m1", tried)
	if first == nil {
		t.Fatal("首次应有账号")
	}
	tried[first.Identity()] = true
	second := p.PickExcluding("", "m1", tried)
	if second == nil || second.Identity() == first.Identity() {
		t.Fatalf("应换到没试过的账号，实得 %v（首个 %s）", nameOf(second), first.Name())
	}
	tried[second.Identity()] = true
	if third := p.PickExcluding("", "m1", tried); third != nil {
		t.Fatalf("全部试过后应返回 nil，实得 %v", nameOf(third))
	}
}

// ---------------------------------------------------------------------------
// 台账
// ---------------------------------------------------------------------------

func TestIdentityStableAndDistinct(t *testing.T) {
	// 同一账号（同 accountId/uid/mobile）换个文件名，身份指纹必须不变
	a1 := &Account{Name: "x", Raw: map[string]any{
		"accountId": "acc-fixed", "uid": "u1", "mobile": "13800138000"}}
	a2 := &Account{Name: "renamed", Raw: map[string]any{
		"accountId": "acc-fixed", "uid": "u1", "mobile": "13800138000"}}
	if a1.Identity() != a2.Identity() {
		t.Fatal("同一账号换文件名后身份指纹必须不变")
	}
	a3 := &Account{Name: "x", Raw: map[string]any{
		"accountId": "acc-fixed", "uid": "u2", "mobile": "13800138000"}}
	if a1.Identity() == a3.Identity() {
		t.Fatal("不同账号的身份指纹必须不同")
	}
	if len(a1.Identity()) != 16 {
		t.Fatalf("身份指纹长度应为 16，实得 %d", len(a1.Identity()))
	}
}

func TestCheckinIdempotentPerDay(t *testing.T) {
	l := newLedger(filepath.Join(t.TempDir(), "state.json"))
	id := "abc"
	day := "2026-10-01"
	if l.CheckinDone(id, day) {
		t.Fatal("初始应为未签到")
	}
	l.MarkCheckin(id, day, true, 2, "领取 2 项")
	if !l.CheckinDone(id, day) {
		t.Fatal("标记后应为已签到")
	}
	if l.CheckinDone(id, "2026-10-02") {
		t.Fatal("不同日期应各自独立")
	}
	// 同日重复写入应保留较大战果，不被后一次覆盖
	l.MarkCheckin(id, day, true, 0, "重跑")
	if c := l.CheckinOf(id); c.Claimed != 2 {
		t.Fatalf("同日重跑不该把战果从 2 覆盖成 %d", c.Claimed)
	}
}

func TestLedgerPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	l := newLedger(path)
	l.UpdateCredits("id1", 1234, []Segment{{Source: "activity", Remaining: 1234,
		ExpiresAt: 1791407259}})

	l2 := newLedger(path)
	c := l2.CreditsOf("id1")
	if c == nil {
		t.Fatal("重载后应读回积分")
	}
	if f, _ := toFloat(c.Total); f != 1234 {
		t.Fatalf("合计应为 1234，实得 %v", c.Total)
	}
	if c.SoonestExpiry != 1791407259 {
		t.Fatalf("最早到期应为 1791407259，实得 %v", c.SoonestExpiry)
	}
}

// ---------------------------------------------------------------------------
// 解析
// ---------------------------------------------------------------------------

func TestParseTSToleratesFormats(t *testing.T) {
	base := parseTS("2026-10-07T21:07:39")
	if base <= 0 {
		t.Fatalf("基准值解析失败: %v", base)
	}
	// 同一时刻的不同表达必须归一到同一秒
	for _, v := range []any{
		"2026-10-07T21:07:39", base * 1000, int(base), int64(base), int32(base),
	} {
		if got := parseTS(v); got != base {
			t.Fatalf("输入 %#v 应解析为 %v，实得 %v", v, base, got)
		}
	}
	for _, v := range []any{nil, "", "垃圾"} {
		if got := parseTS(v); got != 0 {
			t.Fatalf("无效输入 %#v 应返回 0，实得 %v", v, got)
		}
	}
}

func TestMultiplierFreeDetection(t *testing.T) {
	free := []any{"x0", "x0.0", "0", 0, 0.0, "X0"}
	paid := []any{"x1.0", "x2.0", 1, 2.5, nil, "", "x"}
	for _, v := range free {
		if !multiplierIsFree(v) {
			t.Fatalf("%v 应判为零计费", v)
		}
	}
	for _, v := range paid {
		if multiplierIsFree(v) {
			t.Fatalf("%v 不该判为零计费", v)
		}
	}
}

func TestCollectCreditsSegments(t *testing.T) {
	raw := map[string]any{
		"totalAmount": 3100, "totalBalance": 3099,
		"memberTotal": 0, "memberBalance": 0,
		"activityTotal": 3100, "activityBalance": 3099,
		"activityNextExpireTime": "2026-10-07T21:07:39",
	}
	total, segs := collectCredits(raw)
	if f, _ := toFloat(total); f != 3099 {
		t.Fatalf("合计应为 3099（用 balance 而非 amount），实得 %v", total)
	}
	var found bool
	for _, s := range segs {
		if s.Source == "activity" && s.ExpiresAt > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("应解析出 activity 段及其到期时间")
	}
	if got := soonestExpiry(segs); got != parseTS("2026-10-07T21:07:39") {
		t.Fatalf("最早到期应等于 activity 段，实得 %v", got)
	}
}

func TestSessionKeyPrefersHeader(t *testing.T) {
	p := testPool(t, []*Account{acct("a", "u1", "1")})
	_ = p
	if sessionKey(newReqWithHeader(t, "x-session-id", "abc"), map[string]any{"user": "u"}) != "abc" {
		t.Fatal("会话键应优先取显式请求头")
	}
	if sessionKey(newReqWithHeader(t, "", ""), map[string]any{"user": "u"}) != "user:u" {
		t.Fatal("无请求头时应回落到 body.user")
	}
	if sessionKey(newReqWithHeader(t, "", ""), map[string]any{}) != "" {
		t.Fatal("都没有时应为空串")
	}
}

func TestMaskHidesSecrets(t *testing.T) {
	s := mask("13800138000")
	if strings.Contains(s, "600000") {
		t.Fatalf("脱敏后不该露出中段：%s", s)
	}
	if mask("") != "" {
		t.Fatal("空串应原样返回")
	}
}

func TestSummaryNeverLeaksToken(t *testing.T) {
	a := acct("a", "u1", "13800138000")
	b, _ := json.Marshal(a.Summary())
	if strings.Contains(string(b), "bearer-a") || strings.Contains(string(b), "sso-a") {
		t.Fatalf("账号摘要泄露了凭据: %s", b)
	}
}

// ---------------------------------------------------------------------------
// 服务端
// ---------------------------------------------------------------------------

func TestChatFailsFastWhenNoAccount(t *testing.T) {
	// 没有任何可用账号时必须快速返回错误，而不是挂住等上游
	dir := t.TempDir()
	cfg := Config{AccountsDir: filepath.Join(dir, "accounts"), StatePath: filepath.Join(dir, "s.json")}
	_ = os.MkdirAll(cfg.AccountsDir, 0o700)
	gw := &Gateway{cfg: cfg, pool: newPool(cfg, newLedger(cfg.StatePath))}

	srv := newTestServer(t, gw.Handler())
	start := time.Now()
	resp, err := srv.Client().Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("无账号时不应返回 2xx，实得 %d", resp.StatusCode)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("无账号时应快速失败，实际耗时 %s", d)
	}
}

func TestAuthGateRejectsBadKey(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{APIKey: "secret", AccountsDir: filepath.Join(dir, "a"), StatePath: filepath.Join(dir, "s.json")}
	gw := &Gateway{cfg: cfg, pool: newPool(cfg, newLedger(cfg.StatePath))}
	srv := newTestServer(t, gw.Handler())

	resp, err := srv.Client().Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("无 key 应 401，实得 %d", resp.StatusCode)
	}
}

func nameOf(s *Slot) string {
	if s == nil {
		return "<nil>"
	}
	return s.Name()
}

// newReqWithHeader 造一个带指定请求头的请求（header 为空则不带）。
func newReqWithHeader(t *testing.T, header, value string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	if header != "" {
		r.Header.Set(header, value)
	}
	return r
}

// newTestServer 起一个测试用 HTTP 服务，结束时自动关闭。
func newTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// TestAccountConcurrentAccess 上游巡检会同时读写同一账号的 Raw，
// 没有互斥会触发 Go 的并发 map 读写 fatal error（进程级崩溃，不可恢复）。
func TestAccountConcurrentAccess(t *testing.T) {
	a := acct("a", "u1", "13800138000")
	a.Path = filepath.Join(t.TempDir(), "a.json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			a.setStr("modelBearerToken", "rotated-"+string(rune('a'+i%26)))
			_ = a.Bearer()
			_ = a.UID()
			_ = a.Cookie()
			_ = a.Identity()
			_ = a.StudioHeaders()
			_ = a.Summary()
		}
	}()
	for i := 0; i < 300; i++ {
		if err := a.Save(); err != nil {
			t.Fatalf("Save 失败: %v", err)
		}
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("账号并发读写超时")
	}
}

// TestConcurrentCheckinAcrossAccounts 多账号并发签到时台账不能被写坏。
func TestConcurrentCheckinAcrossAccounts(t *testing.T) {
	var accts []*Account
	for i := 0; i < 8; i++ {
		accts = append(accts, acct("a"+string(rune('0'+i)), "u"+string(rune('0'+i)),
			"1860000000"+string(rune('0'+i))))
	}
	p := testPool(t, accts)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			for _, s := range p.Slots() {
				id := s.Identity()
				p.ledger.MarkCheckin(id, "2026-10-01", true, i%3, "并发写")
				_ = p.ledger.CheckinDone(id, "2026-10-01")
				_ = p.ledger.CreditsOf(id)
				_ = p.ledger.Snapshot()
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("台账并发写入超时")
	}
	// 重载后必须仍是合法 JSON
	l2 := newLedger(p.ledger.path)
	if len(l2.Snapshot()) != 8 {
		t.Fatalf("重载后应有 8 个账号记录，实得 %d", len(l2.Snapshot()))
	}
}

// ---------------------------------------------------------------------------
// 登录态失效必须被识别 —— 讯飞把鉴权失败放在 body 的 code 里（HTTP 200）
//
// 回归背景：ssoSessionId 失效时上游返回
//   HTTP 200 {"flag":false,"code":80000,"desc":"登录异常，请重新登录"}
// studioGet 当时只把 HTTP 401/403 当鉴权错误，于是：
//   isAuthErr=false → 槽位不进冷却 → /health 继续报 healthy
//   → 池子继续往死账号派单。健康数变成假话，且是静默的。
// ---------------------------------------------------------------------------

// 造一个上游：始终返回 HTTP 200 + body 里的鉴权错误码。
func authDeadUpstream(t *testing.T, body string) *Upstream {
	t.Helper()
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, body)
	}))
	a := acct("a", "u1", "1")
	u := newUpstream(a, srv.Client())
	return u
}

func TestBodyAuthCodeIsTreatedAsAuthExpiry(t *testing.T) {
	// 上游约定的登录态失效码，必须被识别成“凭据失效”。
	for _, code := range []string{"80000"} {
		u := authDeadUpstream(t, `{"flag":false,"code":`+code+`,"desc":"登录异常，请重新登录"}`)
		_, err := u.SyncModels(context.Background())
		if err == nil {
			t.Fatalf("code=%s 应返回错误", code)
		}
		if !isAuthErr(err) {
			t.Fatalf("code=%s 必须被判定为凭据失效（否则死账号会被当成健康），实得: %v", code, err)
		}
	}
}

func TestDeadCredentialNotReportedHealthy(t *testing.T) {
	// 凭据失效后：不健康 + 不被派单 + /health 单独列出待重登账号。
	u := authDeadUpstream(t, `{"flag":false,"code":80000,"desc":"登录异常，请重新登录"}`)
	p := testPool(t, []*Account{acct("a", "u1", "1")})
	s := p.Find("a")
	s.Up = u
	seed(p, s, 100, nil, []Model{{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase}})

	if _, err := p.RefreshModels(context.Background(), s); err == nil {
		t.Fatal("上游凭据失效，RefreshModels 应报错")
	}
	if s.Healthy() {
		t.Fatal("凭据失效的账号不能被算作健康")
	}
	if !s.CredDead() {
		t.Fatal("应被标记为待重新登录")
	}
	if got := p.Pick("", "m1"); got != nil {
		t.Fatalf("凭据失效的账号不该再被派单，实得 %v", nameOf(got))
	}

	// 冷却会自然过期，但“待重新登录”是状态不是计时器：过点后仍须为不健康。
	s.mu.Lock()
	s.failUntil = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	if s.Healthy() {
		t.Fatal("冷却过期后仍不得把待重登账号报成健康")
	}
	if got := p.Pick("", "m1"); got != nil {
		t.Fatalf("冷却过期后也不该派单给待重登账号，实得 %v", nameOf(got))
	}
}

func TestHealthEscalatesWhenAllCredentialsDead(t *testing.T) {
	u := authDeadUpstream(t, `{"flag":false,"code":80000,"desc":"登录异常，请重新登录"}`)
	p := testPool(t, []*Account{acct("a", "u1", "1")})
	s := p.Find("a")
	s.Up = u
	seed(p, s, 100, nil, []Model{{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase}})
	if _, err := p.RefreshModels(context.Background(), s); err != nil {
		// 这里只关心槽位状态，错误本身已在上面的测试断言
		_ = err
	}

	cfg := Config{AccountsDir: p.cfg.AccountsDir, StatePath: p.cfg.StatePath}
	gw := &Gateway{cfg: cfg, pool: p}
	srv := newTestServer(t, gw.Handler())
	resp, err := srv.Client().Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["healthy"] != float64(0) {
		t.Fatalf("全部凭据失效时 healthy 应为 0，实得 %v", got["healthy"])
	}
	if got["status"] != "degraded" {
		t.Fatalf("全部凭据失效时 status 应为 degraded，实得 %v", got["status"])
	}
	rl, _ := got["needs_relogin"].([]any)
	if len(rl) != 1 || rl[0] != "a" {
		t.Fatalf("应列出待重登账号 [a]，实得 %v", got["needs_relogin"])
	}
}

func TestReloadWithNewSessionRevivesDeadSlot(t *testing.T) {
	// 换上新 session（重新登录）后应自动复活；只换 modelBearerToken 不算。
	u := authDeadUpstream(t, `{"flag":false,"code":80000,"desc":"登录异常，请重新登录"}`)
	p := testPool(t, []*Account{acct("a", "u1", "1")})
	s := p.Find("a")
	s.Up = u
	seed(p, s, 100, nil, []Model{{ID: "m1", Multiplier: "x1.0", BaseURL: DefaultMaasBase}})
	_, _ = p.RefreshModels(context.Background(), s)
	if !s.CredDead() {
		t.Fatal("前置条件：应已标记待重登")
	}

	// 仅换 bearer（上游定期轮换）→ 不得复活
	rot := acct("a", "u1", "1")
	rot.Raw["modelBearerToken"] = "bearer-rotated"
	if rot.SessionMaterial() != s.Account.SessionMaterial() {
		t.Fatal("前置条件：换 bearer 不应改变 session 指纹")
	}
	s.Account = rot
	s.Up.account = rot
	p.Reload()
	if !p.Find("a").CredDead() {
		t.Fatal("只轮换 bearer 不该让账号复活（登录态并未恢复）")
	}

	// 换 ssoSessionId（真的重新登录）→ 应复活
	fresh := acct("a", "u1", "1")
	fresh.Raw["ssoSessionId"] = "sso-brand-new"
	fresh.Path = p.cfg.AccountsDir + string(filepath.Separator) + "a.json"
	if err := fresh.Save(); err != nil {
		t.Fatal(err)
	}
	p.Reload()
	if p.Find("a").CredDead() {
		t.Fatal("换上新 session 后应自动复活")
	}
}
