"""单个 AStudio 账号的上游调用：模型目录、积分、签到、推理。

本模块只关心「一个账号怎么跟上游说话」；多账号的选择/调度在 pool.py。
HTTP 客户端由池子共享（连接复用），本模块不持有自己的 client。
"""

from __future__ import annotations

import time
from typing import Any

import httpx

from .accounts import Account
from .consts import (
    BUILTIN_MODELS,
    DEFAULT_MAAS_BASE,
    MODELS_CONFIG_PATH,
    STUDIO_BASE,
    log,
)


class AuthExpired(Exception):
    """控制面登录凭据失效（401/403），需要重新登录 AStudio 客户端。

    注意：这只用于控制面（模型目录/积分）——那里的 403 表示整条会话不可用。
    推理面的 403 是「该账号对这个模型没有订购」（11200），属模型级权限，
    不走这里，见 Pool.note_status。
    """


class Upstream:
    """一个账号的上游视图。"""

    def __init__(self, account: Account, client: httpx.AsyncClient) -> None:
        self.account = account
        self._c = client
        self.models: list[dict[str, Any]] = list(BUILTIN_MODELS)
        self.models_synced_at: float = 0.0
        self.last_error: str | None = None

    # -- 基础请求 ----------------------------------------------------------
    async def _studio_get(self, path: str, **kw: Any) -> Any:
        r = await self._c.get(STUDIO_BASE + path, headers=self.account.studio_headers(), **kw)
        if r.status_code in (401, 403):
            raise AuthExpired(f"HTTP {r.status_code}")
        r.raise_for_status()
        j = r.json()
        if j.get("flag") is False and j.get("code") not in (0, None):
            raise RuntimeError(f"{j.get('desc')} (code={j.get('code')})")
        return j

    async def _studio_post(self, path: str, body: dict[str, Any] | None = None) -> Any:
        h = self.account.studio_headers()
        h["Content-Type"] = "application/json"
        r = await self._c.post(STUDIO_BASE + path, headers=h, json=body or {})
        if r.status_code in (401, 403):
            raise AuthExpired(f"HTTP {r.status_code}")
        r.raise_for_status()
        return r.json()

    # -- 模型目录 ----------------------------------------------------------
    def model_by_id(self, mid: str) -> dict[str, Any] | None:
        low = (mid or "").lower()
        for m in self.models:
            if m["id"].lower() == low or str(m.get("name", "")).lower() == low:
                return m
        return None

    def resolve(self, mid: str) -> dict[str, Any]:
        hit = self.model_by_id(mid)
        if hit:
            return hit
        return {"id": mid, "name": mid, "base_url": DEFAULT_MAAS_BASE,
                "multiplier": None, "api_key": None}

    def supports(self, mid: str) -> bool:
        """该账号的服务端目录里有没有这个模型。

        目录未同步过（只有兜底表）时不阻拦 —— 交给上游判，避免刚启动全判不支持。
        """
        if self.models_synced_at == 0.0:
            return True
        return self.model_by_id(mid) is not None

    async def sync_models(self) -> list[dict[str, Any]]:
        """拉服务端模型清单（同时验证 cookie 是否有效），并取回各模型 api_key。"""
        r = await self._c.get(STUDIO_BASE + MODELS_CONFIG_PATH,
                              headers=self.account.studio_headers())
        if r.status_code in (401, 403):
            raise AuthExpired(f"HTTP {r.status_code}")
        r.raise_for_status()
        payload = r.json()
        code = payload.get("code")
        if code not in (0, None, "0", "None") :
            raise RuntimeError(f"上游返回 code={code} desc={payload.get('desc')}")

        items = payload.get("data") or []
        out: list[dict[str, Any]] = []
        for it in items:
            mid = str(it.get("model") or "").strip()
            if not mid:
                continue
            out.append({
                "id": mid,
                "name": str(it.get("name") or mid).strip(),
                "multiplier": it.get("point_multiplier"),
                "base_url": str(it.get("base_url") or DEFAULT_MAAS_BASE).rstrip("/"),
                "provider": it.get("provider"),
                "is_default": bool(it.get("is_default")),
                "is_current": bool(it.get("is_current")),
                "api_key": str(it.get("api_key") or "").strip() or None,
            })
        if not out:
            raise RuntimeError("上游模型清单为空")

        known = {m["id"] for m in out}
        for m in BUILTIN_MODELS:
            if m["id"] not in known:
                out.append(dict(m))

        self.models = out
        self.models_synced_at = time.time()
        self.last_error = None

        # 服务端下发的 api_key 若与本地不同 → 轮换了，立刻换用新的
        fresh = next((m["api_key"] for m in out if m.get("is_current") and m.get("api_key")), None) \
            or next((m["api_key"] for m in out if m.get("api_key")), None)
        if fresh and fresh != self.account.bearer:
            self.account.raw["modelBearerToken"] = fresh
            try:
                self.account.save()
                log("[%s] 检测到 api_key 轮换，已写回账号文件", self.account.name)
            except OSError as e:
                log("[%s] api_key 写回失败(内存生效): %s", self.account.name, e)
        return self.models

    # -- 积分 --------------------------------------------------------------
    async def points_balance(self) -> dict[str, Any]:
        return (await self._studio_get("points/balance")).get("data", {}) or {}

    async def points_details(self, page: int = 1, size: int = 20) -> dict[str, Any]:
        return (await self._studio_get(
            f"points/details?pageNum={page}&pageSize={size}")).get("data", {}) or {}

    async def points_summary(self) -> dict[str, Any]:
        return (await self._studio_get("points/summary")).get("data", {}) or {}

    def collect_credits(self, balance: dict[str, Any] | None = None) -> dict[str, Any]:
        """把 points/balance 的扁平字段整理成分段结构（供池子排序）。"""
        b = balance if balance is not None else {}
        if not b:
            return {"total": None, "segments": [], "raw": {}}

        segments: list[dict[str, Any]] = []
        # 字段形如 activityTotal / activityBalance / activityNextExpireTime
        for prefix in ("member", "buy", "activity", "spark", "gift", "bonus"):
            total = b.get(f"{prefix}Total")
            remain = b.get(f"{prefix}Balance")
            exp = b.get(f"{prefix}NextExpireTime")
            if total is None and remain is None:
                continue
            segments.append({
                "source": prefix,
                "total": total,
                "remaining": remain,
                "expires_at": _norm_ts(exp),
            })
        if not segments:
            segments.append({
                "source": "total",
                "total": b.get("totalAmount"),
                "remaining": b.get("totalBalance"),
                "expires_at": None,
            })
        return {
            "total": b.get("totalBalance", b.get("totalAmount")),
            "totalAmount": b.get("totalAmount"),
            "segments": segments,
            "raw": b,
        }

    async def fetch_credits(self) -> dict[str, Any]:
        bal = await self.points_balance()
        return self.collect_credits(bal)

    # -- 签到 / 领奖 -------------------------------------------------------
    async def pending_popups(self) -> list[dict[str, Any]]:
        j = await self._studio_get("client-popups/pending")
        d = j.get("data")
        return d if isinstance(d, list) else []

    async def claim_download_reward(self) -> dict[str, Any]:
        return await self._studio_post("client-download-reward/claim")

    async def complete_popup(self, popup_id: int, instance_key: str) -> dict[str, Any]:
        return await self._studio_post("client-popups/complete",
                                       {"popupId": popup_id, "instanceKey": instance_key})

    async def sign_in(self) -> dict[str, Any]:
        """真正的签到：POST tenant-app/v2/init-app。

        这是账号拿到每日积分的那一步（客户端启动登录时调的就是它）。
        一次发两笔，按天幂等：
          - 常规每日登录积分（体验版 100 / 标准版 200 / 高级·畅享 400）
          - 活动期加成，如 2026 国庆（10-01~10-07）额外 5000 星火积分

        注意：client-popups/pending 那些弹窗及其 claim / complete 都是空操作
        —— 实测连早已领过的账号调 claim 也返回成功且余额不变，
        客户端自己也不调 claim（直接显示 success）。它们只打扫 UI 状态。
        """
        return await self._studio_post("tenant-app/v2/init-app")

    async def checkin_once(self, dry: bool = False) -> dict[str, Any]:
        """跑一轮领奖：真正签到(init-app) → 打扫弹窗 → 读余额算战果。"""
        out: dict[str, Any] = {
            "ok": True, "claimed": [], "errors": [],
            "balance_before": None, "balance_after": None,
            "spark_before": None, "spark_after": None,
            "gained": 0, "spark_gained": 0, "balance": None, "spark_balance": None,
            "banned": False, "skipped": dry,
        }

        def _snapshot(p: dict[str, Any]) -> tuple[Any, Any, Any, Any]:
            """读全四本数：累计发放/当前可用 × 通用/星火。

            计量口径：totalAmount 是「累计发放」（单调递增），算战果用它；
            totalBalance 是「当前可用」，会被模型调用消耗。
            早期版本优先读 totalBalance，账号当天花过分就把战果算成负数/0；
            星火积分又不在 totalAmount 里，必须单独取，否则漏报成 +0。
            """
            return (_num(p.get("totalAmount")), _num(p.get("totalBalance")),
                    _num(p.get("sparkTotalAmount")), _num(p.get("sparkTotalBalance")))

        try:
            before = await self.points_balance()
            (out["balance_before"], out["balance"],
             out["spark_before"], out["spark_balance"]) = _snapshot(before)
        except AuthExpired:
            raise
        except Exception as e:  # noqa: BLE001
            out["errors"].append(f"读余额失败: {e}")

        # ① 真正的签到必须先做——发分的是它，不是弹窗
        if not dry:
            try:
                r = await self.sign_in()
                data = r.get("data") if isinstance(r.get("data"), dict) else {}
                out["banned"] = bool(data.get("banned"))
                code = r.get("code")
                out["claimed"].append({"type": "SIGN_IN", "code": code,
                                       "desc": r.get("desc"), "banned": out["banned"]})
                if code not in (0, None, "0"):
                    out["errors"].append(f"签到返回 code={code} desc={r.get('desc')}")
                elif out["banned"]:
                    out["errors"].append("账号已被封禁（banned=true）")
            except AuthExpired:
                raise
            except Exception as e:  # noqa: BLE001
                out["errors"].append(f"签到(init-app)失败: {e}")

        try:
            popups = await self.pending_popups()
        except AuthExpired:
            raise
        except Exception as e:  # noqa: BLE001
            out["errors"].append(f"拉待领弹窗失败: {e}")
            out["ok"] = False
            return out

        out["popups"] = [
            {"popupId": p.get("popupId"), "componentType": p.get("componentType"),
             "popupType": p.get("popupType")}
            for p in popups
        ]

        if not dry:
            for p in popups:
                pid, ikey = p.get("popupId"), p.get("instanceKey") or ""
                ctype = p.get("componentType")
                if not isinstance(pid, int) or not ikey:
                    out["errors"].append(f"条目不完整，跳过: {p}")
                    continue
                try:
                    # 空操作，只打扫 UI 状态；不要在它们身上找积分
                    if ctype == "CLIENT_DOWNLOAD_REWARD_DIALOG":
                        r = await self.claim_download_reward()
                    else:
                        r = await self.complete_popup(pid, ikey)
                    code = r.get("code")
                    out["claimed"].append({"type": ctype, "popupId": pid,
                                           "code": code, "desc": r.get("desc")})
                    if code not in (0, None, "0"):
                        out["errors"].append(f"{ctype} 返回 code={code} desc={r.get('desc')}")
                except AuthExpired:
                    raise
                except Exception as e:  # noqa: BLE001
                    out["errors"].append(f"{ctype}(popupId={pid}) 领取失败: {e}")

        # 收尾：用「累计发放」的差算战果（不会被消耗干扰）
        try:
            after = await self.points_balance()
            (out["balance_after"], out["balance"],
             out["spark_after"], out["spark_balance"]) = _snapshot(after)
        except Exception as e:  # noqa: BLE001
            out["errors"].append(f"读余额失败: {e}")

        if out["balance_before"] is not None and out["balance_after"] is not None:
            out["gained"] = out["balance_after"] - out["balance_before"]
        if out["spark_before"] is not None and out["spark_after"] is not None:
            out["spark_gained"] = out["spark_after"] - out["spark_before"]
        out["ok"] = not out["errors"]
        return out

    @staticmethod
    def signin_succeeded(res: dict[str, Any]) -> bool:
        """这一轮是不是真的完成了签到。

        判据不是「跑过了」，而是「签到那一步确实成功了」：
        必须留下 SIGN_IN 记录且整轮无错误。dry-run 天然为 False。
        旧版只看有没有 claimed，结果空操作也能占掉当日名额（毒化台账）。
        """
        if not res or res.get("errors"):
            return False
        return any(str(c.get("type")) == "SIGN_IN" for c in (res.get("claimed") or []))

    # -- 推理 --------------------------------------------------------------
    def _chat_target(self, body: dict[str, Any]) -> tuple[str, str]:
        entry = self.resolve(str(body.get("model") or ""))
        key = entry.get("api_key") or self.account.bearer
        url = str(entry["base_url"]).rstrip("/") + "/chat/completions"
        return url, key

    def _responses_target(self, body: dict[str, Any]) -> tuple[str, str]:
        entry = self.resolve(str(body.get("model") or ""))
        key = entry.get("api_key") or self.account.bearer
        url = str(entry["base_url"]).rstrip("/") + "/responses"
        return url, key

    def chat(self, body: dict[str, Any], stream: bool) -> Any:
        url, key = self._chat_target(body)
        h = self.account.maas_headers(key)
        h["Content-Type"] = "application/json"
        if not stream:
            return self._c.post(url, headers=h, json=body)
        return self._c.stream("POST", url, headers=h, json=body)

    def responses(self, body: dict[str, Any], stream: bool) -> Any:
        url, key = self._responses_target(body)
        h = self.account.maas_headers(key)
        h["Content-Type"] = "application/json"
        if not stream:
            return self._c.post(url, headers=h, json=body)
        return self._c.stream("POST", url, headers=h, json=body)


def _num(v: Any) -> float | None:
    try:
        return float(v) if v is not None else None
    except (TypeError, ValueError):
        return None


def _norm_ts(v: Any) -> float | None:
    from .ledger import _parse_ts
    return _parse_ts(v)
