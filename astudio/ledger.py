"""积分/签到/模型目录台账（持久化到 state.json）。

设计直接对标 codebuddy2api 的 CreditLedger + ModelCatalogCache：
- 按 **账号身份指纹** 记账，不是按文件名 —— 换了凭据文件不会把旧余额算到新号头上。
- 积分段带 `expires_at`，供池子做「快过期优先」排序。
- 签到按 `(身份, 日期)` 幂等，重复调用不会重复请求上游。
"""

from __future__ import annotations

import json
import os
import threading
import time
from copy import deepcopy
from pathlib import Path
from typing import Any

from .consts import MODELS_TTL, STATE_PATH, log


def _atomic_write(path: Path, data: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
    os.replace(tmp, path)
    try:
        os.chmod(path, 0o600)
    except OSError:
        pass


def _parse_ts(v: Any) -> float | None:
    """把 '2026-10-07T21:07:39' / 毫秒数 / 秒数 统一成秒级 unix。"""
    if v in (None, ""):
        return None
    if isinstance(v, (int, float)):
        n = float(v)
        return n / 1000.0 if n > 1e11 else n
    s = str(v).strip()
    try:
        return _parse_ts(float(s))
    except ValueError:
        pass
    for fmt in ("%Y-%m-%dT%H:%M:%S", "%Y-%m-%d %H:%M:%S", "%Y-%m-%dT%H:%M:%S.%f"):
        try:
            import datetime as _dt
            return _dt.datetime.strptime(s[: len(fmt) + 6].strip(), fmt).replace(
                tzinfo=_dt.timezone.utc).timestamp()
        except Exception:  # noqa: BLE001
            continue
    return None


def soonest_expiry(segments: list[dict[str, Any]] | None) -> float | None:
    """一组积分段里最早的过期时间；无数据返回 None。"""
    exps = [s.get("expires_at") for s in (segments or []) if s.get("expires_at")]
    return min(exps) if exps else None


class Ledger:
    """{identity: {checkin, credits, error, models}} 持久化台账。"""

    def __init__(self, path: Path | None = None) -> None:
        self.path = Path(path or STATE_PATH)
        self._lock = threading.RLock()
        self._data: dict[str, Any] = {"version": 2, "accounts": {}}
        self._load()

    # -- 存取 --------------------------------------------------------------
    def _load(self) -> None:
        try:
            d = json.loads(self.path.read_text(encoding="utf-8"))
            if isinstance(d, dict) and isinstance(d.get("accounts"), dict):
                self._data = d
        except Exception:  # noqa: BLE001
            self._data = {"version": 2, "accounts": {}}

    def _save(self) -> None:
        try:
            _atomic_write(self.path, self._data)
        except OSError as e:
            log("台账写入失败: %s", e)

    def _entry(self, identity: str) -> dict[str, Any]:
        return self._data["accounts"].setdefault(
            identity, {"checkin": {}, "credits": {}, "error": None, "models": {}})

    def entry(self, identity: str) -> dict[str, Any]:
        with self._lock:
            return deepcopy(self._data["accounts"].get(identity) or {})

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            return deepcopy(self._data["accounts"])

    def bind_identity(self, identity: str, name: str) -> None:
        """记录身份 ↔ 账号名的对应，并在身份变化时清掉旧账（防止串账）。"""
        with self._lock:
            e = self._entry(identity)
            e["name"] = name
            self._save()

    def remove(self, identity: str) -> None:
        with self._lock:
            self._data["accounts"].pop(identity, None)
            self._save()

    # -- 积分 --------------------------------------------------------------
    def update_credits(self, identity: str, result: dict[str, Any]) -> None:
        with self._lock:
            e = self._entry(identity)
            e["credits"] = {
                "total": result.get("total"),
                "segments": deepcopy(result.get("segments") or []),
                "soonest_expiry": soonest_expiry(result.get("segments")),
                "fetched_at": time.time(),
                "raw": result.get("raw"),
            }
            e["error"] = None
            self._save()

    def soonest_expiry_of(self, identity: str) -> float | None:
        with self._lock:
            c = (self._data["accounts"].get(identity) or {}).get("credits") or {}
            return c.get("soonest_expiry") or soonest_expiry(c.get("segments"))

    def credits_of(self, identity: str) -> dict[str, Any]:
        with self._lock:
            return deepcopy((self._data["accounts"].get(identity) or {}).get("credits") or {})

    def note_error(self, identity: str, message: str) -> None:
        with self._lock:
            self._entry(identity)["error"] = str(message)[:300]
            self._save()

    # -- 签到 --------------------------------------------------------------
    def checkin_done(self, identity: str, day: str) -> bool:
        with self._lock:
            c = (self._data["accounts"].get(identity) or {}).get("checkin") or {}
            if c.get("date") != day or c.get("ok") is not True:
                return False
            # 自愈：旧版 checkin 只调 client-popups/* 那几个空操作接口，一分没拿到
            # 却把当天写成 ok=true/claimed=0，然后整个当天再也不重试——账号静默漏签。
            # claimed==0 的成功记录只可能来自那条老路径（现在签到成功必然带 SIGN_IN
            # 记录，claimed>=1），所以直接判为未签到，让下一次自然补签。
            return int(c.get("claimed") or 0) > 0

    def mark_checkin(self, identity: str, day: str, ok: bool, *, claimed: int = 0,
                     detail: str = "") -> None:
        with self._lock:
            e = self._entry(identity)
            prev = e.get("checkin") or {}
            # 同一天重复写入时保留最大的领取数量，避免后一次覆盖前一次战果
            claimed = max(int(claimed), int(prev.get("claimed") or 0)) if prev.get("date") == day else int(claimed)
            e["checkin"] = {"date": day, "ok": bool(ok), "claimed": claimed,
                            "detail": str(detail)[:300], "at": time.time()}
            self._save()

    # -- 模型目录 ----------------------------------------------------------
    def put_models(self, identity: str, models: list[dict[str, Any]]) -> None:
        with self._lock:
            e = self._entry(identity)
            e["models"] = {"list": deepcopy(models), "at": time.time()}
            self._save()

    def models_of(self, identity: str, ttl: float = MODELS_TTL) -> list[dict[str, Any]] | None:
        """TTL 内返回缓存的模型目录；过期或没有则返回 None。"""
        with self._lock:
            m = (self._data["accounts"].get(identity) or {}).get("models") or {}
            if not m.get("list"):
                return None
            if time.time() - float(m.get("at") or 0) > ttl:
                return None
            return deepcopy(m["list"])
