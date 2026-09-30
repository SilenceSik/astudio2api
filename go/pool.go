package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Slot 池里的一个账号槽位。
type Slot struct {
	Account *Account
	Up      *Upstream

	mu        sync.Mutex
	failUntil time.Time
	modelFail map[string]time.Time
	lastErr   string

	// credDead：凭据已确认失效，需要重新登录。与 failUntil 不同 —— 这是
	// 状态不是计时器，没有自然过期时间，只有换上新凭据才会清除。
	//
	// 为什么必须有它：讯飞 session 死掉时，/health 曾把死账号报成健康
	// （冷却过点后又变「健康」），池子还会继续往它派单。健康数必须是
	// 真话，所以这个标记不做超时。
	credDead bool
	credWhy  string
}

func (s *Slot) Identity() string { return s.Account.Identity() }
func (s *Slot) Name() string     { return s.Account.Name }

// CredDead 报告该槽位是否处于「凭据失效，待重新登录」状态。
func (s *Slot) CredDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credDead
}

func (s *Slot) MarkCredDead(reason string) {
	s.mu.Lock()
	s.credDead = true
	s.credWhy = reason
	s.lastErr = reason
	if u := time.Now().Add(CooldownAuth); u.After(s.failUntil) {
		s.failUntil = u
	}
	s.mu.Unlock()
}

func (s *Slot) ClearCredDead() {
	s.mu.Lock()
	s.credDead = false
	s.credWhy = ""
	s.mu.Unlock()
}

func (s *Slot) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.credDead {
		return false
	}
	return time.Now().After(s.failUntil)
}

func (s *Slot) ModelHealthy(mid string) bool {
	if mid == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.modelFail[mid]
	return !ok || time.Now().After(until)
}

func (s *Slot) Cooldown(d time.Duration, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	until := time.Now().Add(d)
	if until.After(s.failUntil) {
		s.failUntil = until
	}
	if reason != "" {
		s.lastErr = reason
	}
}

func (s *Slot) CooldownModel(mid string, d time.Duration) {
	if mid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelFail == nil {
		s.modelFail = map[string]time.Time{}
	}
	until := time.Now().Add(d)
	if cur, ok := s.modelFail[mid]; !ok || until.After(cur) {
		s.modelFail[mid] = until
	}
}

func (s *Slot) NoteOK() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failUntil = time.Time{}
	s.lastErr = ""
}

func (s *Slot) Status() (cooldownSec float64, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := time.Until(s.failUntil).Seconds()
	if d < 0 {
		d = 0
	}
	return d, s.lastErr
}

// ---------------------------------------------------------------------------
// 池
// ---------------------------------------------------------------------------

type Pool struct {
	cfg    Config
	ledger *Ledger
	client *httpClient

	mu      sync.Mutex
	slots   []*Slot
	rr      map[string]int
	sticky  map[string]stickyEntry
	skOrder []string
	loadErr []string
}

type stickyEntry struct {
	identity string
	at       time.Time
}

func newPool(cfg Config, ledger *Ledger) *Pool {
	return &Pool{
		cfg: cfg, ledger: ledger,
		client: newHTTPClient(),
		rr:     map[string]int{},
		sticky: map[string]stickyEntry{},
	}
}

// slotsLocked 内部访问器：调用方必须已持有 p.mu。
func (p *Pool) slotsLocked() []*Slot {
	out := make([]*Slot, len(p.slots))
	copy(out, p.slots)
	return out
}

// Slots 返回槽位快照（自带加锁）。
func (p *Pool) Slots() []*Slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slotsLocked()
}

func (p *Pool) Find(name string) *Slot {
	for _, s := range p.Slots() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func (p *Pool) LoadErrors() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.loadErr...)
}

// Reload 重新扫描账号目录并重建槽位（保留同名账号的状态）。
func (p *Pool) Reload() {
	accs, errs := loadAll(p.cfg.AccountsDir)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadErr = errs
	old := map[string]*Slot{}
	for _, s := range p.slots {
		old[s.Name()] = s
	}
	var fresh []*Slot
	for _, a := range accs {
		if prev, ok := old[a.Name]; ok && prev.Identity() == a.Identity() {
			// 同一账号：若凭据（session/token）换了，说明是重新登录过的新凭据，
			// 清掉「待重新登录」标记让它复活。只换 modelBearerToken 不算换凭据，
			// 那是上游定期轮换的下发 token，不代表登录态恢复。
			if prev.Account.SessionMaterial() != a.SessionMaterial() {
				prev.ClearCredDead()
			}
			prev.Account = a
			prev.Up.account = a
			fresh = append(fresh, prev)
		} else {
			fresh = append(fresh, &Slot{Account: a, Up: newUpstream(a, p.client.HTTP)})
		}
	}
	p.slots = fresh
	for _, s := range fresh {
		p.ledger.BindIdentity(s.Identity(), s.Name())
	}
}

// ---------------------------------------------------------------------------
// 选择
// ---------------------------------------------------------------------------

func (p *Pool) usableModels(s *Slot) []Model {
	if cached := p.ledger.ModelsOf(s.Identity(), ModelsTTL); cached != nil {
		return cached
	}
	return s.Up.Models()
}

func (p *Pool) supports(s *Slot, mid string) bool {
	if mid == "" {
		return true
	}
	if cached := p.ledger.ModelsOf(s.Identity(), ModelsTTL); cached != nil {
		low := strings.ToLower(mid)
		for _, m := range cached {
			if strings.ToLower(m.ID) == low {
				return true
			}
		}
		return false
	}
	return s.Up.Supports(mid)
}

// modelFree 该账号目录里此模型是否零计费。
func (p *Pool) modelFree(s *Slot, mid string) bool {
	if mid == "" || strings.EqualFold(mid, "astronclaw-auto") {
		return false
	}
	for _, m := range p.usableModels(s) {
		if strings.EqualFold(m.ID, mid) {
			return multiplierIsFree(m.Multiplier)
		}
	}
	return false
}

func (p *Pool) creditsTotal(s *Slot) (float64, bool) {
	c := p.ledger.CreditsOf(s.Identity())
	if c == nil || c.Total == nil {
		return 0, false
	}
	return toFloat(c.Total)
}

// expiryRank 快过期优先：无数据排最后，其次按最早到期升序。
func (p *Pool) expiryRank(s *Slot) (bool, float64) {
	e := p.ledger.SoonestExpiryOf(s.Identity())
	return e == 0, e
}

func (p *Pool) eligible(s *Slot, mid string) bool {
	if !s.Healthy() {
		return false
	}
	if !s.ModelHealthy(mid) {
		return false
	}
	if !p.supports(s, mid) {
		return false
	}
	if mid != "" && p.modelFree(s, mid) {
		return true // 免费模型：零余额也能用
	}
	if total, ok := p.creditsTotal(s); ok && total <= 0 {
		return false // 零余额退出付费模型
	}
	return true
}

// candidatesLocked 内部版：调用方必须已持有 p.mu。
func (p *Pool) candidatesLocked(mid string) []*Slot {
	var out []*Slot
	for _, s := range p.slotsLocked() {
		if p.eligible(s, mid) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool {
		fi, fj := p.modelFree(out[i], mid), p.modelFree(out[j], mid)
		if fi != fj {
			return fi // 免费优先
		}
		ni, ei := p.expiryRank(out[i])
		nj, ej := p.expiryRank(out[j])
		if ni != nj {
			return !ni // 有数据的排前
		}
		return ei < ej // 快过期优先
	})
	return out
}

// candidates 公开版：自带加锁。
func (p *Pool) candidates(mid string) []*Slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.candidatesLocked(mid)
}

func (p *Pool) evictStickyLocked() {
	now := time.Now()
	for len(p.skOrder) > 0 {
		k := p.skOrder[0]
		e, ok := p.sticky[k]
		if !ok || now.Sub(e.at) > StickyTTL || len(p.skOrder) > StickyMax {
			delete(p.sticky, k)
			p.skOrder = p.skOrder[1:]
			continue
		}
		break
	}
}

// Pick 按黏绑选账号；无可用则返回 nil（上层快速失败，不打上游）。
func (p *Pool) Pick(skey string, mid string) *Slot {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictStickyLocked()

	cands := p.candidatesLocked(mid)
	if len(cands) == 0 {
		if skey != "" {
			delete(p.sticky, skey)
		}
		return nil
	}
	best := cands[0]
	free := p.modelFree(best, mid)
	ni, ei := p.expiryRank(best)
	var top []*Slot
	for _, s := range cands {
		n, e := p.expiryRank(s)
		if p.modelFree(s, mid) == free && n == ni && e == ei {
			top = append(top, s)
		}
	}
	if skey != "" {
		if e, ok := p.sticky[skey]; ok {
			for _, s := range top {
				if s.Identity() == e.identity {
					e.at = time.Now()
					p.sticky[skey] = e
					return s
				}
			}
		}
	}
	key := mid
	if key == "" {
		key = "-"
	}
	i := p.rr[key] % len(top)
	p.rr[key]++
	if skey != "" {
		p.sticky[skey] = stickyEntry{identity: top[i].Identity(), at: time.Now()}
		p.skOrder = append(p.skOrder, skey)
	}
	return top[i]
}

// pickExcluding 选一个没试过的账号。
func (p *Pool) PickExcluding(skey, mid string, tried map[string]bool) *Slot {
	if s := p.Pick(skey, mid); s != nil && !tried[s.Identity()] {
		return s
	}
	for _, s := range p.candidates(mid) {
		if !tried[s.Identity()] {
			return s
		}
	}
	return nil
}

// NoteStatus 按真实响应码记冷却。
func (p *Pool) NoteStatus(s *Slot, status int, mid string) {
	switch {
	case status == 401:
		// 凭据真的失效了——整个账号下线
		s.Cooldown(CooldownAuth, "HTTP 401 登录失效")
		logf("[%s] 凭据失效(HTTP 401)，冷却 %.0fs", s.Name(), CooldownAuth.Seconds())
	case status == 403:
		// 403 = 该账号对这个模型没有订购/额度（11200 permission_error）。
		// 这是账号×模型的权限属性，不是凭据问题——只摘掉这一个模型，
		// 别把账号上其它能用的模型一起拖下水。
		if mid != "" {
			s.CooldownModel(mid, CooldownEntitlement)
			logf("[%s] 模型 %s 无订购权限(HTTP 403)，该模型冷却 %.0fs",
				s.Name(), mid, CooldownEntitlement.Seconds())
		} else {
			s.Cooldown(CooldownAuth, "HTTP 403")
		}
	case status == 429:
		if mid != "" {
			s.CooldownModel(mid, CooldownRate)
		} else {
			s.Cooldown(CooldownRate, "HTTP 429 限流")
		}
	case status >= 500:
		s.Cooldown(CooldownError, fmt.Sprintf("HTTP %d", status))
	}
}

// ---------------------------------------------------------------------------
// 同步
// ---------------------------------------------------------------------------

// RefreshModels 刷新单账号模型目录并落台账。
func (p *Pool) RefreshModels(ctx context.Context, s *Slot) ([]Model, error) {
	models, err := s.Up.SyncModels(ctx)
	if err != nil {
		if isAuthErr(err) {
			s.MarkCredDead("登录失效，需重新登录 AStudio 客户端")
			p.ledger.NoteError(s.Identity(), "登录失效，需重新登录 AStudio 客户端")
		} else {
			p.ledger.NoteError(s.Identity(), err.Error())
		}
		return nil, err
	}
	s.ClearCredDead()
	p.ledger.PutModels(s.Identity(), models)
	p.ledger.NoteError(s.Identity(), "")
	return models, nil
}

// RefreshCredits 刷新单账号积分。
func (p *Pool) RefreshCredits(ctx context.Context, s *Slot) error {
	total, segs, err := s.Up.FetchCredits(ctx)
	if err != nil {
		if isAuthErr(err) {
			s.MarkCredDead("登录失效，需重新登录 AStudio 客户端")
		}
		p.ledger.NoteError(s.Identity(), err.Error())
		return err
	}
	p.ledger.UpdateCredits(s.Identity(), total, segs)
	return nil
}

// SyncResult 一轮同步的结果。
type SyncResult struct {
	Models  map[string]int `json:"models"`
	Credits []string       `json:"credits"`
	Errors  []string       `json:"errors"`
}

// SyncAll 并发刷新全部账号的目录/积分（有界并发）。
func (p *Pool) SyncAll(ctx context.Context, models, credits bool, concurrency int) *SyncResult {
	if concurrency < 1 {
		concurrency = 4
	}
	res := &SyncResult{Models: map[string]int{}, Credits: []string{}, Errors: []string{}}
	var mu sync.Mutex
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, s := range p.Slots() {
		wg.Add(1)
		go func(s *Slot) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if models {
				if ms, err := p.RefreshModels(ctx, s); err != nil {
					mu.Lock()
					res.Errors = append(res.Errors, s.Name()+" 目录: "+err.Error())
					mu.Unlock()
				} else {
					mu.Lock()
					res.Models[s.Name()] = len(ms)
					mu.Unlock()
				}
			}
			if credits {
				if err := p.RefreshCredits(ctx, s); err != nil {
					mu.Lock()
					res.Errors = append(res.Errors, s.Name()+" 积分: "+err.Error())
					mu.Unlock()
				} else {
					mu.Lock()
					res.Credits = append(res.Credits, s.Name())
					mu.Unlock()
				}
			}
		}(s)
	}
	wg.Wait()
	sort.Strings(res.Errors)
	return res
}

// ---------------------------------------------------------------------------
// 签到
// ---------------------------------------------------------------------------

// CheckinAccountResult 单账号签到结果。
type CheckinAccountResult struct {
	Name         string           `json:"name"`
	Skipped      string           `json:"skipped,omitempty"`
	Error        string           `json:"error,omitempty"`
	Claimed      int              `json:"claimed"`
	Gained       float64          `json:"gained"`
	SparkGained  float64          `json:"spark_gained,omitempty"`
	Balance      *float64         `json:"balance,omitempty"`
	SparkBalance *float64         `json:"spark_balance,omitempty"`
	Detail       []map[string]any `json:"detail,omitempty"`
	Errors       []string         `json:"errors,omitempty"`
}

// CheckinAll 全部账号跑一轮领奖；按 (身份, 日期) 幂等。
func (p *Pool) CheckinAll(ctx context.Context, dry, force bool, concurrency int) []CheckinAccountResult {
	if concurrency < 1 {
		concurrency = 2
	}
	day := time.Now().Format("2006-01-02")
	slots := p.Slots()
	out := make([]CheckinAccountResult, len(slots))
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i, s := range slots {
		wg.Add(1)
		go func(i int, s *Slot) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = p.checkinOne(ctx, s, day, dry, force)
		}(i, s)
	}
	wg.Wait()
	return out
}

func (p *Pool) checkinOne(ctx context.Context, s *Slot, day string, dry, force bool) CheckinAccountResult {
	r := CheckinAccountResult{Name: s.Name()}
	if !force && !dry && p.ledger.CheckinDone(s.Identity(), day) {
		r.Skipped = "当日已领"
		return r
	}
	res, err := s.Up.CheckinOnce(ctx, dry)
	if err != nil {
		if isAuthErr(err) {
			r.Error = "登录失效，需重新登录 AStudio 客户端"
			s.MarkCredDead("登录失效，需重新登录 AStudio 客户端")
		} else {
			r.Error = err.Error()
			p.ledger.NoteError(s.Identity(), err.Error())
		}
		if !dry {
			p.ledger.MarkCheckin(s.Identity(), day, false, 0, r.Error)
		}
		return r
	}
	r.Claimed = len(res.Claimed)
	r.Gained = res.Gained
	r.SparkGained = res.SparkGained
	r.Balance = res.Balance
	r.SparkBalance = res.SparkBalance
	r.Detail = res.Claimed
	r.Errors = res.Errors
	if !dry {
		detail := strings.Join(res.Errors, "; ")
		if detail == "" {
			detail = fmt.Sprintf("领取 %d 项", len(res.Claimed))
		}
		// 只有当 init-app 真的跑成功时才占掉当日名额。
		//
		// 这里踩过一次坑：旧版 checkin 只调 client-popups/* 那几个空操作接口，
		// 什么都拿不到却把当天标成「已签到」（ok=true, claimed=0），于是
		// 真正的签到接口再也没机会被调用——台账把当天名额毒化了。
		// 所以判据不是「跑过了」，而是「签到那一步确实成功了」。
		if signinSucceeded(res) {
			p.ledger.MarkCheckin(s.Identity(), day, true, len(res.Claimed), detail)
		} else {
			p.ledger.MarkCheckin(s.Identity(), day, false, len(res.Claimed), detail)
		}
		if len(res.Errors) > 0 {
			p.ledger.NoteError(s.Identity(), detail)
		}
	}
	return r
}

// signinSucceeded 判断这一轮是不是真的完成了签到：
// 必须留下 SIGN_IN 记录（init-app 成功），且整轮没有错误。
// dry-run 不产生 Claimed，天然为 false。
func signinSucceeded(res *CheckinResult) bool {
	if res == nil || len(res.Errors) > 0 {
		return false
	}
	for _, c := range res.Claimed {
		if fmt.Sprint(c["type"]) == "SIGN_IN" {
			return true
		}
	}
	return false
}

func isAuthErr(err error) bool {
	return err != nil && (errors.Is(err, ErrAuthExpired) ||
		strings.Contains(err.Error(), ErrAuthExpired.Error()))
}
