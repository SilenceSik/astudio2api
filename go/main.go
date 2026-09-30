package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const version = "2.0.0"

func usage() {
	fmt.Print(`astudio2api —— 把讯飞 AStudio 客户端账号池变成 OpenAI 兼容 API

用法:
  astudio2api serve [--host H] [--port N]   启动网关（默认 127.0.0.1:8788）
  astudio2api accounts list                 看池里有哪些账号
  astudio2api accounts login  [--account A] [--password P] [--name N]
                             用讯飞账号密码直接登录换会话（推荐）。
                             不给 --password 则交互式免回显输入；密码不落盘。
  astudio2api accounts import [--name N] [--src F] [--overwrite]
                             导入账号（默认用客户端活会话）
  astudio2api accounts remove --name N      移除账号
  astudio2api models                        列出各账号可用的模型
  astudio2api credits                       查各账号积分与到期
  astudio2api checkin [--dry] [--force]     执行一轮签到/领奖
  astudio2api sync                          刷新全部账号的模型目录与积分
  astudio2api doctor                        自检：账号/目录/积分/签到/推理
  astudio2api version

环境变量:
  ASTUDIO_API_KEY        网关访问密钥（设了就要带 Authorization: Bearer）
  ASTUDIO_PORT           监听端口（默认 8788）
  ASTUDIO_ACCOUNTS_DIR   账号目录（默认 <可执行文件目录>/accounts）
  ASTUDIO_STATE_PATH     台账文件（默认 <可执行文件目录>/state.json）
  ASTUDIO_SESSION_PATH   客户端活会话文件路径（默认自动探测）
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	cfg := loadConfig()

	var code int
	switch cmd {
	case "serve":
		code = cmdServe(cfg, args)
	case "accounts":
		code = cmdAccounts(cfg, args)
	case "models":
		code = cmdModels(cfg)
	case "credits":
		code = cmdCredits(cfg)
	case "checkin":
		code = cmdCheckin(cfg, args)
	case "sync":
		code = cmdSync(cfg)
	case "doctor":
		code = cmdDoctor(cfg)
	case "version", "-V", "--version":
		fmt.Printf("astudio2api %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		usage()
		code = 1
	}
	os.Exit(code)
}

// makePool 建池并（首次运行时）自动纳入客户端当前登录的账号。
func makePool(cfg Config) *Pool {
	p := newPool(cfg, newLedger(cfg.StatePath))
	p.Reload()
	if len(p.Slots()) == 0 {
		if a, err := importLiveSession(cfg, "", false); err == nil {
			logf("已从客户端活会话导入首个账号: %s", a.Name)
			p.Reload()
		} else {
			logf("未找到可用账号: %v", err)
		}
	}
	return p
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func cmdServe(cfg Config, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	host := fs.String("host", "127.0.0.1", "监听地址")
	port := fs.Int("port", cfg.Port, "监听端口")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	pool := makePool(cfg)
	gw := &Gateway{cfg: cfg, pool: pool}
	logf("astudio2api v%s 监听 http://%s:%d", version, *host, *port)
	logf("账号池就绪：%d 个账号，%d 个模型", len(pool.Slots()), len(gw.unionModels()))
	if cfg.APIKey == "" {
		logf("警告：未设置 ASTUDIO_API_KEY，网关无鉴权（仅用于本机/内网）")
	}
	for _, e := range pool.LoadErrors() {
		logf("账号加载告警: %s", e)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	wg := gw.StartBackground(ctx)

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", *host, *port),
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logf("服务退出: %v", err)
			stop()
		}
	}()
	<-ctx.Done()
	logf("正在关闭…")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	wg.Wait()
	return 0
}

// ---------------------------------------------------------------------------
// accounts
// ---------------------------------------------------------------------------

func cmdAccounts(cfg Config, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "需要子命令: list | import | login | remove")
		return 1
	}
	switch args[0] {
	case "list":
		accs, errs := loadAll(cfg.AccountsDir)
		for _, a := range accs {
			banned := ""
			if a.Banned() {
				banned = "  [已封禁]"
			}
			uid := a.UID()
			if uid == "" {
				uid = a.Identity()
			}
			fmt.Printf("  %-16s uid=%-14s mobile=%-10s method=%s%s\n",
				a.Name, uid, mask(a.Mobile()), a.str("loginMethod"), banned)
		}
		for _, e := range errs {
			fmt.Printf("  [警告] %s\n", e)
		}
		fmt.Printf("共 %d 个账号，目录 %s\n", len(accs), cfg.AccountsDir)
		return 0

	case "import":
		fs := flag.NewFlagSet("import", flag.ContinueOnError)
		name := fs.String("name", "", "账号名（默认按手机号尾号生成）")
		src := fs.String("src", "", "要导入的会话文件（省略则用客户端活会话）")
		overwrite := fs.Bool("overwrite", false, "同名账号已存在时覆盖")
		if err := fs.Parse(args[1:]); err != nil {
			return 1
		}
		var (
			a   *Account
			err error
		)
		if *src != "" {
			a, err = addAccountFile(cfg, *src, *name, *overwrite)
		} else {
			a, err = importLiveSession(cfg, *name, *overwrite)
		}
		if err != nil {
			logf("导入失败：%v", err)
			return 1
		}
		logf("已导入 %s → %s", a.Name, a.Path)
		return 0

	case "login":
		fs := flag.NewFlagSet("login", flag.ContinueOnError)
		account := fs.String("account", "", "讯飞账号（手机号/邮箱/用户名）")
		password := fs.String("password", "", "密码（不传则交互式免回显输入；不落盘）")
		name := fs.String("name", "", "账号名（默认 acct-<前4位>）")
		overwrite := fs.Bool("overwrite", true, "同名账号已存在时覆盖")
		if err := fs.Parse(args[1:]); err != nil {
			return 1
		}
		acct := strings.TrimSpace(*account)
		if acct == "" {
			fmt.Fprint(os.Stderr, "账号: ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			acct = strings.TrimSpace(line)
		}
		pw := *password
		if pw == "" {
			p, err := promptPassword("密码: ")
			if err != nil {
				logf("读取密码失败：%v", err)
				return 1
			}
			pw = p
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		a, err := LoginAndAddAccount(ctx, cfg, newHTTPClient().HTTP, acct, pw, *name, *overwrite)
		pw = "" // 尽快丢弃引用
		if err != nil {
			logf("登录失败：%v", err)
			return 1
		}
		logf("已登录并落盘 %s → %s", a.Name, a.Path)
		return 0

	case "remove":
		fs := flag.NewFlagSet("remove", flag.ContinueOnError)
		name := fs.String("name", "", "要移除的账号名")
		if err := fs.Parse(args[1:]); err != nil {
			return 1
		}
		if *name == "" {
			fmt.Fprintln(os.Stderr, "需要 --name 指定要移除的账号")
			return 1
		}
		if !removeAccount(cfg, *name) {
			logf("账号不存在：%s", *name)
			return 1
		}
		logf("已移除 %s", *name)
		return 0
	}
	fmt.Fprintf(os.Stderr, "未知子命令: %s\n", args[0])
	return 1
}

// ---------------------------------------------------------------------------
// 展示类命令
// ---------------------------------------------------------------------------

func needAccounts(p *Pool) bool {
	if len(p.Slots()) == 0 {
		logf("池内没有账号。先执行：astudio2api accounts import")
		return false
	}
	return true
}

func cmdModels(cfg Config) int {
	p := makePool(cfg)
	if !needAccounts(p) {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	p.SyncAll(ctx, true, false, 4)
	for _, s := range p.Slots() {
		logf("── %s (%s) ──", s.Name(), orDash(s.Account.UID()))
		ms := p.usableModels(s)
		for _, m := range ms {
			free := ""
			if multiplierIsFree(m.Multiplier) {
				free = " [免费]"
			}
			fmt.Printf("   %-24s %-22s %-8v%s\n", m.ID, m.Name, orDash(fmt.Sprint(m.Multiplier)), free)
		}
		logf("   共 %d 个", len(ms))
	}
	return 0
}

func cmdCredits(cfg Config) int {
	p := makePool(cfg)
	if !needAccounts(p) {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	p.SyncAll(ctx, false, true, 4)
	for _, s := range p.Slots() {
		logf("── %s ──", s.Name())
		c := p.ledger.CreditsOf(s.Identity())
		if c == nil {
			logf("   （无积分数据）")
		} else {
			logf("   合计 %v（%d 段）", c.Total, len(c.Segments))
			for _, seg := range c.Segments {
				fmt.Printf("     %-10s 剩余 %-10v 到期 %s\n",
					seg.Source, seg.Remaining, fmtTS(seg.ExpiresAt))
			}
		}
		if ck := p.ledger.CheckinOf(s.Identity()); ck != nil {
			state := "失败"
			if ck.OK {
				state = "成功"
			}
			logf("   签到 %s → %s（领取 %d 项）", ck.Date, state, ck.Claimed)
		}
		if e := p.ledger.ErrorOf(s.Identity()); e != "" {
			logf("   [错误] %s", e)
		}
	}
	return 0
}

func cmdCheckin(cfg Config, args []string) int {
	fs := flag.NewFlagSet("checkin", flag.ContinueOnError)
	dry := fs.Bool("dry", false, "只看待领项，不实际领取")
	force := fs.Bool("force", false, "忽略当日幂等，强制再跑")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	p := makePool(cfg)
	if !needAccounts(p) {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	bad := 0
	for _, r := range p.CheckinAll(ctx, *dry, *force, 2) {
		switch {
		case r.Skipped != "":
			fmt.Printf("  %-16s 跳过（%s）\n", r.Name, r.Skipped)
		case r.Error != "":
			fmt.Printf("  %-16s 失败：%s\n", r.Name, r.Error)
			bad++
		default:
			note := ""
			if !signinSucceeded(&CheckinResult{Claimed: r.Detail, Errors: r.Errors}) {
				note = "  ⚠ 未确认签到成功（不占当日名额，下次会重试）"
			}
			gain := fmt.Sprintf("%+g", r.Gained)
			if r.SparkGained != 0 {
				gain += fmt.Sprintf("，星火 %+g", r.SparkGained)
			}
			left := ""
			if r.Balance != nil {
				left = fmt.Sprintf("  可用 %g", *r.Balance)
				if r.SparkBalance != nil && *r.SparkBalance != 0 {
					left += fmt.Sprintf(" + 星火 %g", *r.SparkBalance)
				}
			}
			fmt.Printf("  %-16s 领取 %d 项，积分 %s%s%s\n", r.Name, r.Claimed, gain, left, note)
			for _, d := range r.Detail {
				fmt.Printf("      · %v popupId=%v code=%v\n", d["type"], d["popupId"], d["code"])
			}
			for _, e := range r.Errors {
				fmt.Printf("      ! %s\n", e)
			}
		}
	}
	if bad > 0 {
		return 1
	}
	return 0
}

func cmdSync(cfg Config) int {
	p := makePool(cfg)
	if !needAccounts(p) {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	res := p.SyncAll(ctx, true, true, 4)
	names := make([]string, 0, len(res.Models))
	for n := range res.Models {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		logf("  %-16s %d 个模型", n, res.Models[n])
	}
	for _, e := range res.Errors {
		logf("  [错误] %s", e)
	}
	if len(res.Errors) > 0 {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

func cmdDoctor(cfg Config) int {
	ok := true

	logf("① 账号文件")
	accs, errs := loadAll(cfg.AccountsDir)
	if len(accs) == 0 {
		logf("   ✗ accounts/ 下没有账号。先在 AStudio 客户端登录，再 `accounts import`")
		return 2
	}
	for _, a := range accs {
		logf("   ✓ %s uid=%s", a.Name, orDash(a.UID()))
	}
	for _, e := range errs {
		logf("   ! %s", e)
	}

	p := makePool(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	logf("② 模型目录（%d 个账号）", len(p.Slots()))
	for _, s := range p.Slots() {
		ms, err := p.RefreshModels(ctx, s)
		if err != nil {
			logf("   ✗ %s：%v", s.Name(), err)
			ok = false
			continue
		}
		free := 0
		for _, m := range ms {
			if multiplierIsFree(m.Multiplier) {
				free++
			}
		}
		logf("   ✓ %s：%d 个模型（%d 个免费）", s.Name(), len(ms), free)
	}

	logf("③ 积分")
	for _, s := range p.Slots() {
		if err := p.RefreshCredits(ctx, s); err != nil {
			logf("   ✗ %s：%v", s.Name(), err)
			ok = false
			continue
		}
		c := p.ledger.CreditsOf(s.Identity())
		if c == nil {
			logf("   ✗ %s：无数据", s.Name())
			ok = false
			continue
		}
		logf("   ✓ %s：合计 %v（%d 段，最早到期 %s）",
			s.Name(), c.Total, len(c.Segments), fmtTS(c.SoonestExpiry))
	}

	logf("④ 签到（dry-run，只看待领项不实际领取）")
	for _, s := range p.Slots() {
		r, err := s.Up.CheckinOnce(ctx, true)
		if err != nil {
			logf("   ✗ %s：%v", s.Name(), err)
			ok = false
			continue
		}
		note := ""
		if len(r.Errors) > 0 {
			note = "（" + r.Errors[0] + "）"
		}
		logf("   ✓ %s：待领 %d 项%s", s.Name(), len(r.Popups), note)
	}

	logf("⑤ 真实推理")
	var probe *Slot
	for _, s := range p.Slots() {
		if s.Healthy() {
			probe = s
			break
		}
	}
	if probe == nil {
		logf("   ✗ 没有健康账号")
		return 1
	}
	model := pickProbeModel(p, probe)
	resp, err := probe.Up.Chat(ctx, map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": "Reply with exactly: ok"}},
		"max_tokens": 256, "stream": false,
	})
	if err != nil {
		logf("   ✗ %s / %s → %v", probe.Name(), model, err)
		ok = false
	} else {
		defer resp.Body.Close()
		var data map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&data)
		if resp.StatusCode != 200 {
			logf("   ✗ %s / %s → HTTP %d", probe.Name(), model, resp.StatusCode)
			ok = false
		} else {
			msg := map[string]any{}
			if ch, ok2 := data["choices"].([]any); ok2 && len(ch) > 0 {
				if m, ok3 := ch[0].(map[string]any)["message"].(map[string]any); ok3 {
					msg = m
				}
			}
			txt := str(msg["content"])
			if txt == "" {
				txt = str(msg["reasoning_content"])
			}
			if txt == "" {
				logf("   ! %s / %s → 200 但内容为空（推理型模型可能耗尽了 max_tokens）", probe.Name(), model)
			} else {
				logf("   ✓ %s / %s → %q", probe.Name(), model, truncate(txt, 60))
			}
		}
	}

	fmt.Println()
	if ok {
		logf("结论：全部通过 ✓")
		return 0
	}
	logf("结论：有问题 ✗（见上面 ✗ 行）")
	return 1
}

func pickProbeModel(p *Pool, s *Slot) string {
	skip := map[string]bool{"astronclaw-auto": true, "spark-x": true}
	for _, m := range p.usableModels(s) {
		if !skip[m.ID] && !strings.Contains(m.BaseURL, "spark-api-open") {
			return m.ID
		}
	}
	// 一个都挑不出来时退到标准集里的第一个，不要返回 "astronclaw-auto"：
	// auto 已不在对外标准集里，拿它冒烟会把「模型表为空」这个真问题盖掉，
	// 还会让 doctor 在上游 403 时给出「通过」的假结论。
	if b := builtinModels(); len(b) > 0 {
		return b[0].ID
	}
	return ""
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func orDash(s string) string {
	if s == "" || s == "<nil>" {
		return "-"
	}
	return s
}

func str(v any) string {
	if v == nil {
		return ""
	}
	s := fmt.Sprint(v)
	if s == "<nil>" {
		return ""
	}
	return s
}

func fmtTS(ts float64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(int64(ts), 0).Local().Format("2006-01-02 15:04")
}
