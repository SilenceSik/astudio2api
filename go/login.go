package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SSOBase 讯飞统一登录（SSO）服务基址。passport.xfyun.cn 网页登录用的就是它。
// 是变量而非常量：测试要把它指向 mock 服务器。
var SSOBase = "https://sso.xfyun.cn/SSOService"

// SSO 前端把「账号密码登录」映射成两个 GET 接口：
//
//	GET /login/if-captcha?accountName=X
//	     → data:true 表示这次要先过极验滑块，false 表示可直接登录
//	GET /login/check-account?accountName=X&accountPwd=Y&isAct=false
//	     → 成功时 data 里带 ssoSessionId / account_id
//
// 所以凭证可以在服务端直接换出来，不需要浏览器、不需要读本机 cookie 库。
// 这就是为什么本模块存在：让 2api 有一个正经的「填账号密码」入口。
//
// 注意：password 只用于换会话，**不落盘**。

type ssoLoginError struct {
	Code int
	Desc string
}

func (e *ssoLoginError) Error() string { return fmt.Sprintf("%s (code=%d)", e.Desc, e.Code) }

// ssoGet 发一个 GET 并解析 {flag,code,desc,data} 信封。
func ssoGet(ctx context.Context, cl *http.Client, path string, params url.Values) (map[string]any, error) {
	u := SSOBase + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", "https://passport.xfyun.cn/login")
	req.Header.Set("Origin", "https://passport.xfyun.cn")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "+
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")

	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 160))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("响应不是 JSON: %w", err)
	}
	return out, nil
}

// NeedCaptcha 查询该账号这次登录是否需要极验滑块。
func NeedCaptcha(ctx context.Context, cl *http.Client, account string) (bool, error) {
	j, err := ssoGet(ctx, cl, "/login/if-captcha", url.Values{"accountName": {account}})
	if err != nil {
		return false, err
	}
	if v, ok := j["data"].(bool); ok {
		return v, nil
	}
	// data 不是布尔（例如接口改了）时按“需要验证码”处理，宁可拒绝也不盲登。
	return true, nil
}

// SSOLogin 用账号密码换出 ssoSessionId / account_id。
func SSOLogin(ctx context.Context, cl *http.Client, account, password string) (sid, accountID string, err error) {
	account = strings.TrimSpace(account)
	if account == "" || password == "" {
		return "", "", &ssoLoginError{Code: 91001, Desc: "账号或密码为空"}
	}

	needCaptcha, err := NeedCaptcha(ctx, cl, account)
	if err != nil {
		return "", "", fmt.Errorf("查询验证码要求失败: %w", err)
	}
	if needCaptcha {
		// 极验滑块无法在服务端无声通过；明确报错而不是硬闯。
		return "", "", &ssoLoginError{
			Code: 91002,
			Desc: "该账号本次登录要求滑块验证码，请先在浏览器登录一次，或改用 URL/会话导入"}
	}

	j, err := ssoGet(ctx, cl, "/login/check-account", url.Values{
		"accountName": {account},
		"accountPwd":  {password},
		"isAct":       {"false"},
	})
	if err != nil {
		return "", "", err
	}
	if flag, ok := j["flag"].(bool); ok && !flag {
		return "", "", &ssoLoginError{Code: intOf(j["code"]), Desc: strOf(j["desc"], "登录失败")}
	}
	data, _ := j["data"].(map[string]any)
	if data == nil {
		return "", "", fmt.Errorf("登录响应缺少 data")
	}
	sid = strOf(data["ssoSessionId"], "")
	if sid == "" {
		sid = strOf(data["token"], "")
	}
	accountID = strOf(data["account_id"], "")
	if accountID == "" {
		accountID = strOf(data["accountId"], "")
	}
	if sid == "" || accountID == "" {
		return "", "", fmt.Errorf("登录响应里没有 ssoSessionId/account_id")
	}
	return sid, accountID, nil
}

// LoginAndAddAccount 登录 → 验活取 bearer → 落盘。
//
// password 用完即弃，不写进账号文件、不进日志。
func LoginAndAddAccount(ctx context.Context, cfg Config, cl *http.Client,
	account, password, name string, overwrite bool) (*Account, error) {

	sid, accountID, err := SSOLogin(ctx, cl, account, password)
	if err != nil {
		return nil, err
	}

	if name == "" {
		name = "acct-" + firstN(accountID, 4)
	}
	dest := filepath.Join(cfg.AccountsDir, name+".json")
	if _, err := os.Stat(dest); err == nil && !overwrite {
		return nil, fmt.Errorf("账号 %s 已存在（加 --overwrite 覆盖）", name)
	}

	a := &Account{Name: name, Path: dest, Raw: map[string]any{
		"accountId":    accountID,
		"uid":          accountID,
		"token":        sid,
		"ssoSessionId": sid,
		"banned":       false,
		"loginMethod":  "password",
		"loggedInAt":   time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}}

	// 验活 + 取准入 bearer。缺 bearer 的账号池子会直接过滤掉，所以必须拿到。
	up := newUpstream(a, cl)
	models, err := up.SyncModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("登录成功但验活失败: %w", err)
	}
	bearer := ""
	for _, m := range models {
		if m.APIKey != "" {
			bearer = m.APIKey
			break
		}
	}
	if bearer == "" {
		return nil, fmt.Errorf("验活通过但上游未下发 modelBearerToken，账号不入池")
	}
	a.setStr("modelBearerToken", bearer)

	if err := a.Save(); err != nil {
		return nil, err
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func strOf(v any, def string) string {
	if v == nil {
		return def
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "" || s == "<nil>" {
		return def
	}
	return s
}

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	}
	return 0
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
