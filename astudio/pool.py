"""账号池：多账号选择、黏绑、冷却与后台巡检。

调度规则（对标 codebuddy2api 的 CredentialPool）：
  1. **零计费优先** —— 该账号目录里此模型标 x0.00 的，排最前（省钱）。
  2. **快过期优先** —— 同一档内，积分最早到期的排前（避免过期作废）。
  3. **同级轮询** —— 完全同档(零计费相同 + 到期时间相同)的账号之间轮询分摊。
  4. **黏绑** —— 同一会话键在 STICKY_TTL 内固定用同一账号（上游缓存命中），
     但出现更优档位时自动换绑。
  5. **零余额退场** —— 余额为 0 的账号只承接自己目录里标免费的模型。
"""

from __future__ import annotations

import asyncio
import time
from collections import OrderedDict, defaultdict
from typing import Any

import httpx

from . import accounts as acc_mod
from .consts import (
    COOLDOWN_AUTH,
    COOLDOWN_ENTITLEMENT,
    COOLDOWN_ERROR,
    COOLDOWN_RATE,
    MODELS_TTL,
    STICKY_MAX,
    STICKY_TTL,
    log,
    multiplier_is_free,
)
from .ledger import Ledger
from .upstream import AuthExpired, Upstream


class _Slot:
    """池里的一个账号槽位。"""

    __slots__ = ("account", "up", "fail_until", "model_fail", "last_error")

    def __init__(self, account: acc_mod.Account, up: Upstream) -> None:
        self.account = account
        self.up = up
        self.fail_until: float = 0.0                 # 账号级冷却
        self.model_fail: dict[str, float] = {}       # 模型级冷却（429）
        self.last_error: str | None = None

    @property
    def identity(self) -> str:
        return self.account.identity

    @property
    def name(self) -> str:
        return self.account.name

    def healthy(self) -> bool:
        return time.time() >= self.fail_until

    def model_healthy(self, model: str | None) -> bool:
        if not model:
            return True
        return time.time() >= self.model_fail.get(model, 0.0)

    def cooldown(self, seconds: float, reason: str = "") -> None:
        self.fail_until = max(self.fail_until, time.time() + seconds)
        if reason:
            self.last_error = reason

    def cooldown_model(self, model: str, seconds: float) -> None:
        self.model_fail[model] = max(self.model_fail.get(model, 0.0), time.time() + seconds)


class Pool:
    """多账号池。"""

    def __init__(self, ledger: Ledger | None = None) -> None:
        self.ledger = ledger or Ledger()
        self._client: httpx.AsyncClient | None = None
        self._slots: list[_Slot] = []
        self._lock = asyncio.Lock()
        self._rr: dict[str, int] = defaultdict(int)
        self._sticky: OrderedDict[str, tuple[str, float]] = OrderedDict()
        self._reload_errs: list[str] = []

    # -- 生命周期 ----------------------------------------------------------
    async def start(self) -> None:
        self._client = httpx.AsyncClient(
            timeout=httpx.Timeout(600.0, connect=30.0),
            limits=httpx.Limits(max_connections=128, max_keepalive_connections=32),
        )
        await self.reload()

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    def client(self) -> httpx.AsyncClient:
        if self._client is None:
            raise RuntimeError("池子未启动")
        return self._client

    async def reload(self) -> None:
        """重新扫描 accounts/ 并重建槽位（保留同名账号的冷却与目录状态）。"""
        accs, errs = acc_mod.load_all()
        self._reload_errs = errs
        async with self._lock:
            old = {s.account.name: s for s in self._slots}
            fresh: list[_Slot] = []
            for acc in accs:
                prev = old.get(acc.name)
                if prev is not None and prev.account.identity == acc.identity:
                    prev.account = acc
                    prev.up.account = acc
                    fresh.append(prev)
                else:
                    fresh.append(_Slot(acc, Upstream(acc, self.client())))
            self._slots = fresh
            for s in self._slots:
                self.ledger.bind_identity(s.identity, s.name)
        if errs:
            for e in errs:
                log("账号加载告警: %s", e)

    # -- 快照 --------------------------------------------------------------
    def slots(self) -> list[_Slot]:
        return list(self._slots)

    def errors(self) -> list[str]:
        return list(self._reload_errs)

    def find(self, name: str) -> _Slot | None:
        for s in self._slots:
            if s.name == name:
                return s
        return None

    def summary(self) -> list[dict[str, Any]]:
        now = time.time()
        out: list[dict[str, Any]] = []
        for s in self._slots:
            cred = self.ledger.credits_of(s.identity)
            ck = (self.ledger.entry(s.identity).get("checkin") or {})
            out.append({
                "name": s.name,
                "identity": s.identity,
                "uid": s.account.uid or "",
                "models": len(s.up.models),
                "models_synced": bool(s.up.models_synced_at),
                "healthy": s.healthy(),
                "cooldown_s": round(max(0.0, s.fail_until - now), 1),
                "last_error": s.last_error,
                "credits": cred.get("total"),
                "segments": len(cred.get("segments") or []),
                "soonest_expiry": cred.get("soonest_expiry"),
                "checkin": ck or None,
                "banned": s.account.banned,
            })
        return out

    # -- 选择 --------------------------------------------------------------
    def _usable_models(self, slot: _Slot) -> list[dict[str, Any]]:
        cached = self.ledger.models_of(slot.identity, MODELS_TTL)
        if cached:
            return cached
        return slot.up.models

    def _supports(self, slot: _Slot, model: str | None) -> bool:
        if not model:
            return True
        cached = self.ledger.models_of(slot.identity, MODELS_TTL)
        if cached:
            return any(m["id"].lower() == model.lower() for m in cached)
        return slot.up.supports(model)

    def _model_free(self, slot: _Slot, model: str | None) -> bool:
        """该账号目录里此模型是否零计费。"""
        if not model or model.lower() == "astronclaw-auto":
            return False
        for m in self._usable_models(slot):
            if m["id"].lower() == model.lower():
                return multiplier_is_free(m.get("multiplier"))
        return False

    def _credits_total(self, slot: _Slot) -> float | None:
        c = self.ledger.credits_of(slot.identity)
        try:
            return float(c.get("total")) if c.get("total") is not None else None
        except (TypeError, ValueError):
            return None

    def _expiry_rank(self, slot: _Slot) -> tuple[bool, float]:
        """快过期优先：无数据排最后，其次按最早到期升序。"""
        exp = self.ledger.soonest_expiry_of(slot.identity)
        return (exp is None, exp or 0.0)

    def _eligible(self, slot: _Slot, model: str | None) -> bool:
        if not slot.healthy():
            return False
        if not slot.model_healthy(model):
            return False
        if not self._supports(slot, model):
            return False
        if model and self._model_free(slot, model):
            return True                     # 免费模型：零余额也能用
        total = self._credits_total(slot)
        if total is not None and total <= 0:
            return False                    # 零余额退出付费模型
        return True

    def _candidates(self, model: str | None) -> list[_Slot]:
        cands = [s for s in self._slots if self._eligible(s, model)]
        if not cands:
            return []
        cands.sort(key=lambda s: (not self._model_free(s, model), *self._expiry_rank(s)))
        return cands

    def _evict_sticky(self) -> None:
        now = time.time()
        while self._sticky:
            k, (_, ts) = next(iter(self._sticky.items()))
            if now - ts > STICKY_TTL or len(self._sticky) > STICKY_MAX:
                self._sticky.pop(k, None)
            else:
                break

    def pick(self, skey: str | None, model: str | None = None) -> _Slot | None:
        """按黏绑选账号；无可用则返回 None（上层快速失败，不打上游）。"""
        self._evict_sticky()
        cands = self._candidates(model)
        if not cands:
            if skey:
                self._sticky.pop(skey, None)
            return None

        best = cands[0]
        free = self._model_free(best, model)
        rank = self._expiry_rank(best)
        top = [s for s in cands if self._model_free(s, model) == free and self._expiry_rank(s) == rank]

        if skey and skey in self._sticky:
            cid, _ = self._sticky[skey]
            sticky = next((s for s in top if s.identity == cid), None)
            if sticky is not None:
                self._sticky[skey] = (cid, time.time())
                self._sticky.move_to_end(skey)
                return sticky

        key = model or "-"
        slot = top[self._rr[key] % len(top)]
        self._rr[key] += 1
        if skey:
            self._sticky[skey] = (slot.identity, time.time())
        return slot

    def note_status(self, slot: _Slot, status: int, model: str | None = None) -> None:
        """按真实响应码记冷却。"""
        if status == 401:
            # 凭据真的失效了——整个账号下线
            slot.cooldown(COOLDOWN_AUTH, "HTTP 401 登录失效")
            log("[%s] 凭据失效(HTTP 401)，冷却 %.0fs", slot.name, COOLDOWN_AUTH)
        elif status == 403:
            # 403 = 该账号对这个模型没有订购/额度（11200 permission_error）。
            # 这是账号×模型的权限属性，不是凭据问题——只摘掉这一个模型。
            if model:
                slot.cooldown_model(model, COOLDOWN_ENTITLEMENT)
                log("[%s] 模型 %s 无订购权限(HTTP 403)，该模型冷却 %.0fs",
                    slot.name, model, COOLDOWN_ENTITLEMENT)
            else:
                slot.cooldown(COOLDOWN_AUTH, "HTTP 403")
        elif status == 429:
            if model:
                slot.cooldown_model(model, COOLDOWN_RATE)
            else:
                slot.cooldown(COOLDOWN_RATE, "HTTP 429 限流")
        elif status >= 500:
            slot.cooldown(COOLDOWN_ERROR, f"HTTP {status}")

    def note_ok(self, slot: _Slot) -> None:
        slot.fail_until = 0.0
        slot.last_error = None

    # -- 同步 --------------------------------------------------------------
    async def refresh_models(self, slot: _Slot) -> list[dict[str, Any]]:
        """刷新单账号模型目录并落台账。"""
        try:
            models = await slot.up.sync_models()
            self.ledger.put_models(slot.identity, models)
            self.ledger.note_error(slot.identity, "")
            return models
        except AuthExpired:
            slot.cooldown(COOLDOWN_AUTH, "登录失效")
            self.ledger.note_error(slot.identity, "登录失效，需重新登录 AStudio 客户端")
            raise
        except Exception as e:  # noqa: BLE001
            slot.last_error = str(e)
            self.ledger.note_error(slot.identity, str(e))
            raise

    async def refresh_credits(self, slot: _Slot) -> dict[str, Any]:
        try:
            res = await slot.up.fetch_credits()
            self.ledger.update_credits(slot.identity, res)
            return res
        except AuthExpired:
            slot.cooldown(COOLDOWN_AUTH, "登录失效")
            raise
        except Exception as e:  # noqa: BLE001
            self.ledger.note_error(slot.identity, str(e))
            raise

    async def sync_all(self, *, models: bool = True, credits: bool = True,
                       concurrency: int = 4) -> dict[str, Any]:
        """并发刷新全部账号的目录/积分（有界并发，避免同时打爆上游）。"""
        sem = asyncio.Semaphore(max(1, concurrency))
        out: dict[str, Any] = {"models": {}, "credits": {}, "errors": []}

        async def one(s: _Slot) -> None:
            async with sem:
                if models:
                    try:
                        out["models"][s.name] = len(await self.refresh_models(s))
                    except Exception as e:  # noqa: BLE001
                        out["errors"].append(f"{s.name} 目录: {e}")
                if credits:
                    try:
                        await self.refresh_credits(s)
                    except Exception as e:  # noqa: BLE001
                        out["errors"].append(f"{s.name} 积分: {e}")

        await asyncio.gather(*(one(s) for s in self._slots))
        return out

    # -- 签到 --------------------------------------------------------------
    async def checkin_all(self, *, dry: bool = False, concurrency: int = 2,
                          skip_done: bool = True, force: bool = False) -> list[dict[str, Any]]:
        """全部账号跑一轮领奖；按 (身份, 日期) 幂等，当日已成功过的跳过。

        force=True 时忽略当日幂等，强制再跑一轮（对齐 Go 版语义）。
        """
        from datetime import datetime
        day = datetime.now().strftime("%Y-%m-%d")
        sem = asyncio.Semaphore(max(1, concurrency))
        results: list[dict[str, Any]] = []

        async def one(s: _Slot) -> None:
            async with sem:
                if skip_done and not force and self.ledger.checkin_done(s.identity, day):
                    results.append({"name": s.name, "skipped": "当日已领"})
                    return
                try:
                    r = await s.up.checkin_once(dry=dry)
                    # 只有真的签到成功才占当日名额——旧版拿空操作当成功，
                    # 把当天写成 ok=true/claimed=0，导致当天再不复签（静默漏签）。
                    if not dry:
                        ok = s.up.signin_succeeded(r)
                        self.ledger.mark_checkin(
                            s.identity, day, ok,
                            claimed=len(r.get("claimed") or []),
                            detail="; ".join(r.get("errors") or []) or f"领取 {len(r.get('claimed') or [])} 项")
                    if r.get("errors"):
                        self.ledger.note_error(s.identity, "; ".join(r["errors"]))
                    results.append({"name": s.name,
                                    "claimed": len(r.get("claimed") or []),
                                    "gained": r.get("gained"),
                                    "spark_gained": r.get("spark_gained"),
                                    "balance": r.get("balance"),
                                    "spark_balance": r.get("spark_balance"),
                                    "errors": r.get("errors") or [],
                                    "detail": r.get("claimed") or []})
                except AuthExpired:
                    results.append({"name": s.name, "error": "登录失效，需重新登录 AStudio 客户端"})
                    self.ledger.mark_checkin(s.identity, day, False, detail="登录失效")
                    s.cooldown(COOLDOWN_AUTH, "登录失效")
                except Exception as e:  # noqa: BLE001
                    results.append({"name": s.name, "error": str(e)})
                    self.ledger.note_error(s.identity, str(e))

        await asyncio.gather(*(one(s) for s in self._slots))
        return results
