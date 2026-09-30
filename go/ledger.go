package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Segment 一段积分。
type Segment struct {
	Source    string  `json:"source"`
	Total     any     `json:"total"`
	Remaining any     `json:"remaining"`
	ExpiresAt float64 `json:"expires_at,omitempty"`
}

// CreditInfo 某账号的积分快照。
type CreditInfo struct {
	Total         any       `json:"total"`
	Segments      []Segment `json:"segments"`
	SoonestExpiry float64   `json:"soonest_expiry,omitempty"`
	FetchedAt     float64   `json:"fetched_at,omitempty"`
}

// CheckinInfo 某账号的签到记录（按日幂等）。
type CheckinInfo struct {
	Date    string  `json:"date"`
	OK      bool    `json:"ok"`
	Claimed int     `json:"claimed"`
	Detail  string  `json:"detail,omitempty"`
	At      float64 `json:"at,omitempty"`
}

// Entry 台账里单个账号的记录。
type Entry struct {
	Name    string       `json:"name,omitempty"`
	Checkin *CheckinInfo `json:"checkin,omitempty"`
	Credits *CreditInfo  `json:"credits,omitempty"`
	Error   string       `json:"error,omitempty"`
	Models  *ModelsCache `json:"models,omitempty"`
}

// ModelsCache 模型目录缓存。
type ModelsCache struct {
	List []Model `json:"list"`
	At   float64 `json:"at"`
}

// Ledger 持久化台账 {identity: Entry}。
type Ledger struct {
	path string
	mu   sync.RWMutex
	data map[string]*Entry
}

// parseTS 把 "2026-10-07T21:07:39" / 毫秒 / 秒 统一成秒级 unix。
func parseTS(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		if t > 1e11 {
			return t / 1000
		}
		return t
	case int:
		return parseTS(float64(t))
	case int64:
		return parseTS(float64(t))
	case int32:
		return parseTS(float64(t))
	case uint64:
		return parseTS(float64(t))
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return parseTS(f)
		}
		// 支持带时区与不带时区两种
		for _, layout := range []string{
			"2006-01-02T15:04:05", "2006-01-02 15:04:05",
			time.RFC3339, "2006-01-02T15:04:05.000",
		} {
			if ts, err := time.Parse(layout, s); err == nil {
				return float64(ts.UTC().Unix())
			}
		}
	}
	return 0
}

func soonestExpiry(segs []Segment) float64 {
	var min float64
	for _, s := range segs {
		if s.ExpiresAt > 0 && (min == 0 || s.ExpiresAt < min) {
			min = s.ExpiresAt
		}
	}
	return min
}

func newLedger(path string) *Ledger {
	l := &Ledger{path: path, data: map[string]*Entry{}}
	l.load()
	return l
}

func (l *Ledger) load() {
	b, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	var f struct {
		Accounts map[string]*Entry `json:"accounts"`
	}
	if err := json.Unmarshal(b, &f); err != nil || f.Accounts == nil {
		return
	}
	l.data = f.Accounts
}

func (l *Ledger) saveLocked() {
	b, err := json.MarshalIndent(map[string]any{"version": 2, "accounts": l.data}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, l.path)
}

func (l *Ledger) entry(identity string) *Entry {
	e, ok := l.data[identity]
	if !ok {
		e = &Entry{}
		l.data[identity] = e
	}
	return e
}

func (l *Ledger) Exists(identity string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.data[identity]
	return ok
}

func (l *Ledger) BindIdentity(identity, name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entry(identity).Name = name
	l.saveLocked()
}

func (l *Ledger) Remove(identity string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.data, identity)
	l.saveLocked()
}

func (l *Ledger) UpdateCredits(identity string, total any, segs []Segment) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entry(identity).Credits = &CreditInfo{
		Total: total, Segments: segs, SoonestExpiry: soonestExpiry(segs),
		FetchedAt: float64(time.Now().Unix()),
	}
	l.entry(identity).Error = ""
	l.saveLocked()
}

func (l *Ledger) CreditsOf(identity string) *CreditInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if e, ok := l.data[identity]; ok && e.Credits != nil {
		c := *e.Credits
		return &c
	}
	return nil
}

func (l *Ledger) SoonestExpiryOf(identity string) float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if e, ok := l.data[identity]; ok && e.Credits != nil {
		return e.Credits.SoonestExpiry
	}
	return 0
}

func (l *Ledger) NoteError(identity, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if msg == "" {
		l.entry(identity).Error = ""
	} else {
		l.entry(identity).Error = truncate(msg, 300)
	}
	l.saveLocked()
}

func (l *Ledger) ErrorOf(identity string) string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if e, ok := l.data[identity]; ok {
		return e.Error
	}
	return ""
}

func (l *Ledger) CheckinDone(identity, day string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.data[identity]
	if !ok || e.Checkin == nil || e.Checkin.Date != day || !e.Checkin.OK {
		return false
	}
	// 自愈：旧版 checkin 只调 client-popups/* 那几个空操作接口，一分没拿到
	// 却把当天写成 ok=true/claimed=0，然后整个当天再也不重试——账号静默漏签。
	// claimed==0 的成功记录只可能来自那条老路径（现在签到成功必然带 SIGN_IN
	// 记录，claimed>=1），所以直接判为未签到，让下一次自然补签。
	return e.Checkin.Claimed > 0
}

func (l *Ledger) CheckinOf(identity string) *CheckinInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if e, ok := l.data[identity]; ok && e.Checkin != nil {
		c := *e.Checkin
		return &c
	}
	return nil
}

func (l *Ledger) MarkCheckin(identity, day string, ok bool, claimed int, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(identity)
	// 同日重复写入时保留最大的领取数，避免后一次覆盖前一次战果
	if e.Checkin != nil && e.Checkin.Date == day && e.Checkin.Claimed > claimed {
		claimed = e.Checkin.Claimed
	}
	e.Checkin = &CheckinInfo{Date: day, OK: ok, Claimed: claimed,
		Detail: truncate(detail, 300), At: float64(time.Now().Unix())}
	l.saveLocked()
}

func (l *Ledger) PutModels(identity string, models []Model) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entry(identity).Models = &ModelsCache{List: models, At: float64(time.Now().Unix())}
	l.saveLocked()
}

// ModelsOf TTL 内返回缓存的模型目录；过期或没有则返回 nil。
func (l *Ledger) ModelsOf(identity string, ttl time.Duration) []Model {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.data[identity]
	if !ok || e.Models == nil || len(e.Models.List) == 0 {
		return nil
	}
	if time.Since(time.Unix(int64(e.Models.At), 0)) > ttl {
		return nil
	}
	out := make([]Model, len(e.Models.List))
	copy(out, e.Models.List)
	return out
}

func (l *Ledger) Snapshot() map[string]*Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]*Entry, len(l.data))
	for k, v := range l.data {
		c := *v
		out[k] = &c
	}
	return out
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
