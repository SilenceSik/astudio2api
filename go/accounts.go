package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Account 一个 AStudio 账号凭据。
//
// Raw 会被上游巡检并发读写（同步模型目录时会回写轮换后的 api_key），
// 所以统一经 mu 访问 —— Go 的并发 map 读写会直接 fatal error 终止进程。
type Account struct {
	Name string // 文件名去掉后缀
	Path string

	mu  sync.RWMutex
	Raw map[string]any
}

func (a *Account) str(k string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if v, ok := a.Raw[k]; ok && v != nil {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

// setStr 写回单个字段（api_key 轮换时用）。
func (a *Account) setStr(k, v string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Raw == nil {
		a.Raw = map[string]any{}
	}
	a.Raw[k] = v
}

func (a *Account) snapshotRaw() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]any, len(a.Raw))
	for k, v := range a.Raw {
		out[k] = v
	}
	return out
}

func (a *Account) AccountID() string { return a.str("accountId") }
func (a *Account) UID() string       { return a.str("uid") }
func (a *Account) Mobile() string    { return a.str("mobile") }
func (a *Account) Bearer() string    { return a.str("modelBearerToken") }
func (a *Account) Banned() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	v, _ := a.Raw["banned"].(bool)
	return v
}

// Identity 稳定身份指纹：用于台账绑定，防止换号后旧余额串到新号。
func (a *Account) Identity() string {
	seed := a.AccountID() + "|" + a.UID() + "|" + a.Mobile()
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:16]
}

// SessionMaterial 登录凭据指纹。只有「重新登录」才会改变它；
// modelBearerToken 是上游定期轮换的下发 token，故意不纳入 —— 它的变化
// 不代表登录态恢复，不能拿来当复活依据。
func (a *Account) SessionMaterial() string {
	seed := a.str("ssoSessionId") + "|" + a.str("token")
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:16]
}

func (a *Account) Cookie() string {
	var parts []string
	if sid := a.str("ssoSessionId"); sid != "" {
		parts = append(parts, "ssoSessionId="+sid, "sso_sessionid="+sid)
	}
	parts = append(parts, "account_id="+a.AccountID(), "token="+a.str("token"))
	return strings.Join(parts, "; ")
}

func (a *Account) StudioHeaders() map[string]string {
	return map[string]string{
		"Accept":        "application/json",
		"User-Agent":    "AStudio/" + StudioVersion,
		"clientType":    ClientTypeWin,
		"studioVersion": StudioVersion,
		"Cookie":        a.Cookie(),
		"Content-Type":  "application/json",
	}
}

func (a *Account) MaasHeaders(key string) map[string]string {
	if key == "" {
		key = a.Bearer()
	}
	return map[string]string{
		"Accept":        "application/json",
		"User-Agent":    "AStudio/" + StudioVersion,
		"Authorization": "Bearer " + key,
		"uid":           a.UID(),
		"Content-Type":  "application/json",
	}
}

func (a *Account) Save() error {
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a.snapshotRaw(), "", "  ")
	if err != nil {
		return err
	}
	tmp := a.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.Path)
}

func (a *Account) Summary() map[string]any {
	return map[string]any{
		"name":        a.Name,
		"identity":    a.Identity(),
		"uid":         a.UID(),
		"accountId":   mask(a.AccountID()),
		"bearer":      mask(a.Bearer()),
		"mobile":      mask(a.Mobile()),
		"banned":      a.Banned(),
		"loginMethod": a.str("loginMethod"),
	}
}

// ---------------------------------------------------------------------------
// 账号库
// ---------------------------------------------------------------------------

var reservedStateFiles = map[string]bool{
	"state.json": true, "ledger.json": true, "checkin.json": true,
}

func accountFiles(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".json") ||
			strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".tmp") ||
			reservedStateFiles[n] {
			continue
		}
		out = append(out, filepath.Join(dir, n))
	}
	sort.Strings(out)
	return out
}

func loadAccountFile(p string) (*Account, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("内容不是 JSON 对象: %w", err)
	}
	name := strings.TrimSuffix(filepath.Base(p), ".json")
	return &Account{Name: name, Path: p, Raw: raw}, nil
}

// loadAll 加载全部账号；单个坏文件不影响其余。
func loadAll(dir string) (ok []*Account, errs []string) {
	for _, p := range accountFiles(dir) {
		a, err := loadAccountFile(p)
		if err != nil {
			errs = append(errs, filepath.Base(p)+": "+err.Error())
			continue
		}
		if a.Bearer() == "" {
			errs = append(errs, filepath.Base(p)+": 缺少 modelBearerToken，跳过")
			continue
		}
		ok = append(ok, a)
	}
	return
}

// findLiveSession 找客户端当前写入的活会话文件。
func findLiveSession(cfg Config) string {
	for _, c := range liveSessionCandidates(cfg) {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// importLiveSession 把客户端活会话复制进账号目录。
func importLiveSession(cfg Config, name string, overwrite bool) (*Account, error) {
	src := findLiveSession(cfg)
	if src == "" {
		return nil, fmt.Errorf("找不到 AStudio 客户端会话文件；请先在客户端登录，" +
			"或用 `accounts import --src <文件>` 手动导入")
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("会话文件不是 JSON: %w", err)
	}
	a := &Account{Name: name, Path: src, Raw: raw}
	if a.Name == "" {
		tail := "default"
		if m := a.Mobile(); len(m) >= 4 {
			tail = "acct-" + m[len(m)-4:]
		} else {
			tail = "acct-" + a.Identity()[:4]
		}
		a.Name = tail
	}
	dest := filepath.Join(cfg.AccountsDir, a.Name+".json")
	if _, err := os.Stat(dest); err == nil && !overwrite {
		return nil, fmt.Errorf("账号 %s 已存在（加 --overwrite 覆盖）", a.Name)
	}
	a.Path = dest
	if err := a.Save(); err != nil {
		return nil, err
	}
	return a, nil
}

// addAccountFile 从任意路径导入一份会话文件。
func addAccountFile(cfg Config, src, name string, overwrite bool) (*Account, error) {
	if st, err := os.Stat(src); err != nil || st.IsDir() {
		return nil, fmt.Errorf("文件不存在: %s", src)
	}
	a, err := loadAccountFile(src)
	if err != nil {
		return nil, err
	}
	if name != "" {
		a.Name = name
	}
	dest := filepath.Join(cfg.AccountsDir, a.Name+".json")
	if _, err := os.Stat(dest); err == nil && !overwrite {
		return nil, fmt.Errorf("账号 %s 已存在（加 --overwrite 覆盖）", a.Name)
	}
	if err := os.MkdirAll(cfg.AccountsDir, 0o700); err != nil {
		return nil, err
	}
	a.Path = dest
	if err := a.Save(); err != nil {
		return nil, err
	}
	return a, nil
}

func removeAccount(cfg Config, name string) bool {
	p := filepath.Join(cfg.AccountsDir, name+".json")
	if err := os.Remove(p); err != nil {
		return false
	}
	return true
}
