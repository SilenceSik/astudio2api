"""FastAPI 网关：OpenAI 兼容面 + 管理面 + 后台巡检。"""

from __future__ import annotations

import asyncio
import json
import os
import time
from contextlib import asynccontextmanager
from datetime import datetime
from typing import Any, AsyncIterator

from fastapi import FastAPI, Header, HTTPException, Request
from fastapi.responses import JSONResponse, StreamingResponse

from . import accounts as acc_mod
from .consts import (
    BUILTIN_MODELS,
    CHECKIN_INTERVAL,
    CHECKIN_STARTUP_DELAY,
    MODELS_TTL,
    log,
)
from .ledger import Ledger
from .pool import Pool, _Slot
from .upstream import AuthExpired

POOL: Pool | None = None
TASKS: list[asyncio.Task] = []

API_KEY = (os.environ.get("ASTUDIO_API_KEY") or "").strip()


def pool() -> Pool:
    if POOL is None:
        raise HTTPException(503, "池子尚未就绪")
    return POOL


def auth_or_401(authorization: str | None) -> None:
    if not API_KEY:
        return
    if (authorization or "").strip() != f"Bearer {API_KEY}":
        raise HTTPException(401, "Invalid API key")


def session_key(request: Request, body: dict[str, Any] | None = None) -> str | None:
    """会话键：优先客户端显式头，其次 body.user，最后按对端地址粗分。"""
    for h in ("x-hermes-session-id", "x-session-id", "x-conversation-id"):
        v = request.headers.get(h)
        if v:
            return v.strip()
    if body:
        u = body.get("user")
        if isinstance(u, str) and u.strip():
            return f"user:{u.strip()}"
    return None


# ---------------------------------------------------------------------------
# 后台巡检
# ---------------------------------------------------------------------------

async def _initial_sync(p: Pool) -> None:
    """启动后拉一轮模型目录 + 积分（多账号并发）。"""
    await asyncio.sleep(1.0)
    try:
        res = await p.sync_all(models=True, credits=True)
        for name, n in res["models"].items():
            log("账号 %s: %d 个模型", name, n)
        for e in res["errors"]:
            log("同步告警: %s", e)
    except Exception as e:  # noqa: BLE001
        log("首轮同步失败: %s", e)


async def _checkin_loop(p: Pool) -> None:
    """签到巡检：启动 30s 首签，之后每小时兜底（按日幂等，不重复领奖）。"""
    await asyncio.sleep(CHECKIN_STARTUP_DELAY)
    while True:
        try:
            res = await p.checkin_all()
            for r in res:
                if r.get("skipped"):
                    continue
                if r.get("error"):
                    log("签到[%s] 失败: %s", r["name"], r["error"])
                else:
                    n = r.get("claimed") or 0
                    if n:
                        log("签到[%s] 领取 %d 项，积分 %+g", r["name"], n, r.get("gained") or 0)
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            log("签到巡检异常: %s", e)
        await asyncio.sleep(CHECKIN_INTERVAL)


async def _periodic_refresh(p: Pool) -> None:
    """每小时刷新一次模型目录与积分（供池子排序用）。"""
    while True:
        await asyncio.sleep(MODELS_TTL)
        try:
            await p.sync_all(models=True, credits=True)
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            log("周期刷新异常: %s", e)


async def _session_watcher(p: Pool) -> None:
    """盯客户端活会话文件：换账号登录后自动纳入池子。"""
    last: float = 0.0
    while True:
        try:
            src = acc_mod.find_live_session()
            if src is not None:
                m = src.stat().st_mtime
                if last and m > last:
                    acc = acc_mod.import_live_session(overwrite=True)
                    if p.find(acc.name) is None:
                        await p.reload()
                        log("检测到新账号 %s，已加入池子", acc.name)
                    else:
                        await p.refresh_models(p.find(acc.name))  # type: ignore[arg-type]
                last = m
        except FileExistsError:
            pass
        except asyncio.CancelledError:
            raise
        except Exception as e:  # noqa: BLE001
            log("会话监听异常: %s", e)
        await asyncio.sleep(60)


@asynccontextmanager
async def lifespan(_: FastAPI):
    global POOL  # noqa: PLW0603
    POOL = Pool(Ledger())
    await POOL.start()

    # 首次运行：自动把客户端当前登录的账号纳入池子
    if not POOL.slots():
        try:
            acc = acc_mod.import_live_session()
            await POOL.reload()
            log("已从客户端活会话导入首个账号: %s", acc.name)
        except Exception as e:  # noqa: BLE001
            log("未找到可用账号: %s", e)

    log("账号池就绪：%d 个账号", len(POOL.slots()))
    if not API_KEY:
        log("警告：未设置 ASTUDIO_API_KEY，网关无鉴权（仅限本机使用）")

    TASKS.extend([
        asyncio.create_task(_initial_sync(POOL)),
        asyncio.create_task(_checkin_loop(POOL)),
        asyncio.create_task(_periodic_refresh(POOL)),
        asyncio.create_task(_session_watcher(POOL)),
    ])
    try:
        yield
    finally:
        for t in TASKS:
            t.cancel()
        await asyncio.gather(*TASKS, return_exceptions=True)
        TASKS.clear()
        await POOL.aclose()


app = FastAPI(title="astudio2api", version="2.0.0", lifespan=lifespan)


# ---------------------------------------------------------------------------
# 公共面
# ---------------------------------------------------------------------------

@app.get("/health")
async def health() -> dict[str, Any]:
    p = pool()
    slots = p.slots()
    return {
        "status": "ok" if slots else "degraded",
        "accounts": len(slots),
        "healthy": sum(1 for s in slots if s.healthy()),
        "models": len(_union_models(p)),
    }


@app.get("/v1/models")
async def v1_models(authorization: str | None = Header(None)) -> dict[str, Any]:
    auth_or_401(authorization)
    p = pool()
    now = int(time.time())
    return {"object": "list", "data": [
        {"id": m["id"], "object": "model", "created": now, "owned_by": "xfyun"}
        for m in _union_models(p)
    ]}


def _union_models(p: Pool) -> list[dict[str, Any]]:
    """对外暴露的模型清单：以标准集为骨架，用各账号同步回来的真实倍率补齐。

    不取全池并集：受限模型只存在于老号，并集会把它们暴露给所有人，请求落到
    没有订购的账号上就是 403，还白烧一次该模型的冷却。
    """
    by_id: dict[str, dict[str, Any]] = {m["id"]: dict(m) for m in BUILTIN_MODELS}
    for s in p.slots():
        for m in s.up.models:
            cur = by_id.get(m["id"])
            if cur is None:
                continue
            if m.get("multiplier") is not None:
                cur["multiplier"] = m["multiplier"]
            if m.get("name"):
                cur["name"] = m["name"]
    return sorted(by_id.values(), key=lambda m: m["id"])


async def read_json(request: Request) -> dict[str, Any]:
    try:
        body = await request.body()
    except Exception as e:  # noqa: BLE001
        raise HTTPException(400, f"读取请求体失败: {e}") from e
    if not body:
        raise HTTPException(400, "请求体为空")
    try:
        obj = json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError) as e:
        raise HTTPException(400, f"请求体不是合法 JSON: {e}") from e
    if not isinstance(obj, dict):
        raise HTTPException(400, "请求体必须是 JSON 对象")
    return obj


def _upstream_error(msg: str, status: int = 502) -> JSONResponse:
    return JSONResponse(status_code=status,
                        content={"error": {"message": msg, "type": "upstream_error"}})


# ---------------------------------------------------------------------------
# 推理（池化 + 失败换号重试）
# ---------------------------------------------------------------------------

MAX_ATTEMPTS = 3


@app.post("/v1/chat/completions")
async def v1_chat(request: Request, authorization: str | None = Header(None)) -> Any:
    auth_or_401(authorization)
    p = pool()
    body = await read_json(request)
    stream = bool(body.get("stream"))
    skey = session_key(request, body)
    model = str(body.get("model") or "")

    tried: set[str] = set()
    last_err = "没有可用账号"
    for _ in range(MAX_ATTEMPTS):
        slot = _pick_excluding(p, skey, model, tried)
        if slot is None:
            break
        tried.add(slot.identity)
        if not stream:
            try:
                r = await slot.up.chat(body, False)
            except Exception as e:  # noqa: BLE001
                slot.cooldown(30.0, str(e))
                last_err = str(e)
                continue
            if r.status_code >= 400:
                p.note_status(slot, r.status_code, model)
                last_err = f"HTTP {r.status_code}"
                if r.status_code in (401, 403, 429) or r.status_code >= 500:
                    continue
            else:
                p.note_ok(slot)
            return JSONResponse(status_code=r.status_code, content=_safe_json(r))
        # 流式
        return await _stream_chat(p, slot, body, model, tried, skey)

    return _upstream_error(f"上游调用失败：{last_err}")


async def _stream_chat(p: Pool, slot: _Slot, body: dict[str, Any], model: str,
                       tried: set[str], skey: str | None) -> Any:
    """流式：首个字节前失败可以换号重试；一旦开始吐字节就锁定。"""
    attempts = MAX_ATTEMPTS - len(tried) + 1
    last_err = "流式调用失败"
    while attempts > 0:
        attempts -= 1
        try:
            ctx = slot.up.chat(body, True)
            resp = await ctx.__aenter__()
        except Exception as e:  # noqa: BLE001
            slot.cooldown(30.0, str(e))
            last_err = str(e)
            slot = _pick_excluding(p, skey, model, tried)
            if slot is None:
                break
            tried.add(slot.identity)
            continue
        if resp.status_code >= 400:
            raw = await resp.aread()
            await ctx.__aexit__(None, None, None)
            p.note_status(slot, resp.status_code, model)
            last_err = f"HTTP {resp.status_code}: {raw[:200].decode('utf-8', 'replace')}"
            if resp.status_code in (401, 403, 429) or resp.status_code >= 500:
                slot = _pick_excluding(p, skey, model, tried)
                if slot is None:
                    break
                tried.add(slot.identity)
                continue
            return _upstream_error(last_err, resp.status_code)
        p.note_ok(slot)

        async def body_iter() -> AsyncIterator[bytes]:
            try:
                async for chunk in resp.aiter_bytes():
                    yield chunk
            finally:
                await ctx.__aexit__(None, None, None)

        return StreamingResponse(body_iter(), status_code=resp.status_code,
                                 media_type=resp.headers.get("content-type", "text/event-stream"),
                                 headers={"Cache-Control": "no-cache",
                                          "X-Accel-Buffering": "no"})
    return _upstream_error(f"上游流式调用失败：{last_err}")


def _pick_excluding(p: Pool, skey: str | None, model: str, tried: set[str]) -> _Slot | None:
    """选一个没试过的账号；优先走黏绑，但它已被试过时临时绕过。"""
    slot = p.pick(skey, model)
    if slot is not None and slot.identity not in tried:
        return slot
    for s in p._candidates(model):  # noqa: SLF001 - 同包内部使用
        if s.identity not in tried:
            return s
    return None


def _safe_json(r: Any) -> Any:
    try:
        return r.json()
    except Exception:  # noqa: BLE001
        return {"error": {"message": r.text[:500], "type": "upstream_error"}}


@app.post("/v1/responses")
async def v1_responses(request: Request, authorization: str | None = Header(None)) -> Any:
    auth_or_401(authorization)
    p = pool()
    body = await read_json(request)
    stream = bool(body.get("stream", True))
    skey = session_key(request, body)
    model = str(body.get("model") or "")

    tried: set[str] = set()
    last_err = "没有可用账号"
    for _ in range(MAX_ATTEMPTS):
        slot = _pick_excluding(p, skey, model, tried)
        if slot is None:
            break
        tried.add(slot.identity)
        try:
            if not stream:
                r = await slot.up.responses(body, False)
                if r.status_code >= 400:
                    p.note_status(slot, r.status_code, model)
                    last_err = f"HTTP {r.status_code}"
                    continue
                p.note_ok(slot)
                return JSONResponse(status_code=r.status_code, content=_safe_json(r))
            ctx = slot.up.responses(body, True)
            resp = await ctx.__aenter__()
            if resp.status_code >= 400:
                raw = await resp.aread()
                await ctx.__aexit__(None, None, None)
                p.note_status(slot, resp.status_code, model)
                last_err = f"HTTP {resp.status_code}: {raw[:200].decode('utf-8', 'replace')}"
                continue
            p.note_ok(slot)

            async def it() -> AsyncIterator[bytes]:
                try:
                    async for chunk in resp.aiter_bytes():
                        yield chunk
                finally:
                    await ctx.__aexit__(None, None, None)

            return StreamingResponse(it(), status_code=resp.status_code,
                                     media_type=resp.headers.get("content-type", "text/event-stream"),
                                     headers={"Cache-Control": "no-cache",
                                              "X-Accel-Buffering": "no"})
        except Exception as e:  # noqa: BLE001
            slot.cooldown(30.0, str(e))
            last_err = str(e)
            continue
    return _upstream_error(f"上游调用失败：{last_err}")


# ---------------------------------------------------------------------------
# 管理面
# ---------------------------------------------------------------------------

@app.get("/admin/accounts")
async def admin_accounts(authorization: str | None = Header(None)) -> dict[str, Any]:
    auth_or_401(authorization)
    p = pool()
    return {"count": len(p.slots()), "accounts": p.summary(), "load_errors": p.errors()}


@app.post("/admin/accounts/import")
async def admin_import(authorization: str | None = Header(None),
                       path: str | None = None, name: str | None = None,
                       overwrite: bool = True) -> dict[str, Any]:
    """把客户端活会话（或指定文件）导入为池内账号。"""
    auth_or_401(authorization)
    p = pool()
    try:
        if path:
            from pathlib import Path
            acc = acc_mod.add_account_file(Path(path), name, overwrite=overwrite)
        else:
            acc = acc_mod.import_live_session(name, overwrite=overwrite)
    except FileExistsError as e:
        raise HTTPException(409, str(e)) from e
    except FileNotFoundError as e:
        raise HTTPException(404, str(e)) from e
    await p.reload()
    slot = p.find(acc.name)
    if slot is not None:
        try:
            await p.refresh_models(slot)
            await p.refresh_credits(slot)
        except Exception as e:  # noqa: BLE001
            log("导入后同步失败: %s", e)
    return {"ok": True, "account": acc.summary(), "accounts": len(p.slots())}


@app.delete("/admin/accounts/{name}")
async def admin_remove(name: str, authorization: str | None = Header(None)) -> dict[str, Any]:
    auth_or_401(authorization)
    p = pool()
    slot = p.find(name)
    if slot is not None:
        p.ledger.remove(slot.identity)
    ok = acc_mod.remove_account(name)
    if not ok:
        raise HTTPException(404, f"账号不存在: {name}")
    await p.reload()
    return {"ok": True, "removed": name, "accounts": len(p.slots())}


@app.post("/admin/reload")
async def admin_reload(authorization: str | None = Header(None)) -> dict[str, Any]:
    auth_or_401(authorization)
    p = pool()
    await p.reload()
    return {"ok": True, "accounts": len(p.slots()), "load_errors": p.errors()}


@app.post("/admin/sync")
async def admin_sync(authorization: str | None = Header(None)) -> dict[str, Any]:
    auth_or_401(authorization)
    return await pool().sync_all(models=True, credits=True)


@app.post("/admin/checkin")
async def admin_checkin(authorization: str | None = Header(None),
                        dry: bool = False, force: bool = False) -> dict[str, Any]:
    """手动签到。dry=true 只看待领项不领；force=true 忽略当日幂等。"""
    auth_or_401(authorization)
    res = await pool().checkin_all(dry=dry, skip_done=not force)
    return {"ok": True, "results": res}


@app.get("/admin/credits")
async def admin_credits(authorization: str | None = Header(None)) -> dict[str, Any]:
    """全池积分汇总（读台账缓存；refresh=true 时先刷新）。"""
    auth_or_401(authorization)
    p = pool()
    accounts = []
    total = 0.0
    for s in p.slots():
        cred = p.ledger.credits_of(s.identity)
        t = cred.get("total")
        try:
            total += float(t) if t is not None else 0.0
        except (TypeError, ValueError):
            pass
        accounts.append({
            "name": s.name,
            "identity": s.identity,
            "total": t,
            "segments": cred.get("segments") or [],
            "soonest_expiry": cred.get("soonest_expiry"),
            "fetched_at": cred.get("fetched_at"),
            "checkin": (p.ledger.entry(s.identity).get("checkin") or None),
            "error": (p.ledger.entry(s.identity).get("error") or None),
        })
    return {"total": round(total, 2), "count": len(accounts), "accounts": accounts}


@app.post("/admin/credits/refresh")
async def admin_credits_refresh(authorization: str | None = Header(None)) -> dict[str, Any]:
    auth_or_401(authorization)
    p = pool()
    res = await p.sync_all(models=False, credits=True)
    return {"ok": not res["errors"], "errors": res["errors"],
            "credits": await admin_credits(authorization)}


@app.get("/admin/points")
async def admin_points(authorization: str | None = Header(None),
                       account: str | None = None) -> dict[str, Any]:
    """单账号实时余额（account 省略时取池内第一个）。"""
    auth_or_401(authorization)
    p = pool()
    slot = p.find(account) if account else (p.slots()[0] if p.slots() else None)
    if slot is None:
        raise HTTPException(404, "没有可用账号")
    try:
        bal = await slot.up.points_balance()
    except AuthExpired as e:
        raise HTTPException(401, f"[{slot.name}] 登录失效，请在 AStudio 客户端重新登录") from e
    return {"account": slot.name, "balance": bal}


@app.get("/admin/models")
async def admin_models(authorization: str | None = Header(None)) -> dict[str, Any]:
    """按账号看模型供给，便于排查「某模型谁在提供」。"""
    auth_or_401(authorization)
    p = pool()
    rows = []
    for s in p.slots():
        for m in s.up.models:
            rows.append({"account": s.name, "id": m["id"], "name": m.get("name"),
                         "multiplier": m.get("multiplier")})
    by_model: dict[str, list[str]] = {}
    for r in rows:
        by_model.setdefault(r["id"], []).append(r["account"])
    return {"models": sorted(by_model), "providers": by_model, "rows": rows}
