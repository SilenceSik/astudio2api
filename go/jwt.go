package main

// model-gateway 的 JWT（155 字符 HS256，时效约 3 年）。
//
// 它不是客户端本地签发的：AStudio 只是把服务端发的票存起来。签发接口是
//   POST https://agent.xfyun.cn/xingchen-studio/auth/bearer-token
// 只需要 SSO 会话 cookie（ssoSessionId / account_id），无 body。
// 拿到了就能走 model-gateway 的 Responses 通道，不需要安装客户端。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// FetchModelBearer 用当前账号的 SSO cookie 换一张 model-gateway 票。
// 每次调用都会签发新票（iat 变），所以只在缺失或临近过期时调用。
func (u *Upstream) FetchModelBearer(ctx context.Context) (string, error) {
	r, err := u.studioPost(ctx, "auth/bearer-token", nil)
	if err != nil {
		return "", err
	}
	tok, _ := dataOf(r)["modelBearerToken"].(string)
	return strings.TrimSpace(tok), nil
}

// jwtExpiry 解出 JWT 的 exp；解析不了返回零值（视为已过期，需要换票）。
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	seg := parts[1]
	seg += strings.Repeat("=", (4-len(seg)%4)%4)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(seg)
		if err != nil {
			return time.Time{}
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// jwtNeedsRefresh 票缺失或剩余寿命不足一天时返回 true。
func jwtNeedsRefresh(token string) bool {
	if strings.TrimSpace(token) == "" {
		return true
	}
	exp := jwtExpiry(token)
	if exp.IsZero() {
		return true
	}
	return time.Until(exp) < 24*time.Hour
}

// refreshJWTIfNeeded 对需要 Responses 通道的账号补票并写回账号文件。
func (u *Upstream) refreshJWTIfNeeded(ctx context.Context) {
	if !u.responsesMode() {
		return
	}
	if !jwtNeedsRefresh(u.account.JWT()) {
		return
	}
	tok, err := u.FetchModelBearer(ctx)
	if err != nil || tok == "" {
		return
	}
	u.account.setStr("jwt", tok)
	_ = u.account.Save()
}
