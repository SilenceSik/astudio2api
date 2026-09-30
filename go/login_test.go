package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 账号密码登录（SSO）
//
// 背景：讯飞没有 OAuth，但 passport 前端把「账号密码登录」映射成
//   GET /login/if-captcha?accountName=X          → data: 是否需要滑块
//   GET /login/check-account?accountName&accountPwd&isAct → data.ssoSessionId
// 所以可以在服务端直接换出会话 —— 这是 2api 的正规加号入口。
// ---------------------------------------------------------------------------

// ssoMock 造一个 SSO mock。captcha=true 时要求滑块。
func ssoMock(t *testing.T, captcha bool, wantAccount, wantPwd string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/if-captcha":
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":` +
				map[bool]string{true: "true", false: "false"}[captcha] + `}`))
		case "/login/check-account":
			if captcha {
				t.Error("要求滑块时不该直接调 check-account")
			}
			if r.URL.Query().Get("accountName") != wantAccount ||
				r.URL.Query().Get("accountPwd") != wantPwd {
				_, _ = w.Write([]byte(`{"flag":false,"code":-1,"desc":"用户名或密码错误","data":null}`))
				return
			}
			_, _ = w.Write([]byte(`{"flag":true,"code":0,"desc":"成功","data":` +
				`{"ssoSessionId":"sid-abc","account_id":"22050000003"}}`))
		default:
			t.Errorf("意外的路径: %s", r.URL.Path)
			_, _ = w.Write([]byte(`{"flag":false,"code":20003,"desc":"请求地址错误"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withSSOBase 把 SSOBase 指向 mock，测试结束还原。
func withSSOBase(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := SSOBase
	SSOBase = srv.URL
	t.Cleanup(func() { SSOBase = old })
}

func TestSSOLoginSucceeds(t *testing.T) {
	srv := ssoMock(t, false, "22050000003", "pw-correct")
	withSSOBase(t, srv)

	sid, acct, err := SSOLogin(context.Background(), srv.Client(), "22050000003", "pw-correct")
	if err != nil {
		t.Fatalf("应登录成功，实得 %v", err)
	}
	if sid != "sid-abc" || acct != "22050000003" {
		t.Fatalf("凭证解析错：sid=%q acct=%q", sid, acct)
	}
}

func TestSSOLoginWrongPasswordSurfacesServerMessage(t *testing.T) {
	srv := ssoMock(t, false, "22050000003", "pw-correct")
	withSSOBase(t, srv)

	_, _, err := SSOLogin(context.Background(), srv.Client(), "22050000003", "wrong")
	if err == nil {
		t.Fatal("密码错应报错")
	}
	if !strings.Contains(err.Error(), "用户名或密码错误") {
		t.Fatalf("应透出上游原因，实得 %v", err)
	}
}

func TestSSOLoginRefusesWhenCaptchaRequired(t *testing.T) {
	// 需要滑块时必须明确拒绝，不能硬闯，也不能把它当密码错。
	srv := ssoMock(t, true, "22050000003", "pw-correct")
	withSSOBase(t, srv)

	_, _, err := SSOLogin(context.Background(), srv.Client(), "22050000003", "pw-correct")
	if err == nil {
		t.Fatal("要求滑块时应拒绝")
	}
	if !strings.Contains(err.Error(), "滑块") {
		t.Fatalf("应说明是滑块要求，实得 %v", err)
	}
}

func TestSSOLoginRejectsEmpty(t *testing.T) {
	for _, c := range [][2]string{{"", "pw"}, {"acct", ""}, {"", ""}} {
		if _, _, err := SSOLogin(context.Background(), http.DefaultClient, c[0], c[1]); err == nil {
			t.Fatalf("空账号/密码应被拒: %q/%q", c[0], c[1])
		}
	}
}

// ---------------------------------------------------------------------------
// 端到端：登录 → 验活取 bearer → 落盘
// ---------------------------------------------------------------------------

func TestLoginAndAddAccountWritesCredentialWithBearer(t *testing.T) {
	// SSO mock
	sso := ssoMock(t, false, "22050000003", "pw")
	withSSOBase(t, sso)

	// AStudio 上游 mock：验活时下发 api_key（就是准入 bearer）
	studio := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"flag":true,"code":0,"data":[` +
			`{"model":"m1","name":"M1","api_key":"bearer-from-upstream"}]}`))
	}))
	oldStudio := StudioBase
	StudioBase = studio.URL + "/"
	t.Cleanup(func() { StudioBase = oldStudio })

	dir := t.TempDir()
	cfg := Config{AccountsDir: filepath.Join(dir, "accounts"), StatePath: filepath.Join(dir, "s.json")}

	a, err := LoginAndAddAccount(context.Background(), cfg, studio.Client(), "22050000003", "pw", "", false)
	if err != nil {
		t.Fatalf("应成功，实得 %v", err)
	}
	if a.Name != "acct-2205" {
		t.Fatalf("命名应为 acct-2205，实得 %q", a.Name)
	}
	if a.Bearer() != "bearer-from-upstream" {
		t.Fatalf("必须落盘上游下发的 bearer，实得 %q", a.Bearer())
	}
	if a.str("ssoSessionId") != "sid-abc" {
		t.Fatalf("session 落盘错：%q", a.str("ssoSessionId"))
	}

	// 文件里绝不能出现密码
	b, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "pw") && strings.Contains(string(b), `"password"`) {
		t.Fatal("账号文件里出现了密码字段")
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["password"]; ok {
		t.Fatal("账号文件不得包含 password")
	}
	if raw["loginMethod"] != "password" {
		t.Fatalf("loginMethod 应为 password，实得 %v", raw["loginMethod"])
	}
}

func TestLoginAndAddAccountRefusesWithoutBearer(t *testing.T) {
	// 上游不给 api_key 时不能入池（池子会过滤掉缺 bearer 的账号）。
	sso := ssoMock(t, false, "22050000003", "pw")
	withSSOBase(t, sso)

	studio := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"flag":true,"code":0,"data":[{"model":"m1","name":"M1"}]}`))
	}))
	oldStudio := StudioBase
	StudioBase = studio.URL + "/"
	t.Cleanup(func() { StudioBase = oldStudio })

	dir := t.TempDir()
	cfg := Config{AccountsDir: filepath.Join(dir, "accounts"), StatePath: filepath.Join(dir, "s.json")}

	if _, err := LoginAndAddAccount(context.Background(), cfg, studio.Client(), "22050000003", "pw", "", false); err == nil {
		t.Fatal("没有 bearer 时应拒绝入池")
	}
	if entries, _ := os.ReadDir(cfg.AccountsDir); len(entries) != 0 {
		t.Fatalf("拒绝时不该留下账号文件，实得 %d 个", len(entries))
	}
}
