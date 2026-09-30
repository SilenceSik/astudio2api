"""命令行入口：serve / doctor / accounts / checkin / credits / models / sync。"""

from __future__ import annotations

import argparse
import asyncio
import json
import sys
from pathlib import Path
from typing import Any

from . import __version__, accounts as acc_mod
from .consts import ACCOUNTS_DIR, BUILTIN_MODELS, STATE_PATH, log, multiplier_is_free
from .ledger import Ledger
from .pool import Pool
from .upstream import AuthExpired


def _n(v: Any) -> float:
    try:
        return float(v) if v is not None else 0.0
    except (TypeError, ValueError):
        return 0.0


def _date(ts: Any) -> str:
    if not ts:
        return "-"
    try:
        from datetime import datetime
        return datetime.fromtimestamp(float(ts)).strftime("%Y-%m-%d %H:%M")
    except Exception:  # noqa: BLE001
        return "-"


async def _with_pool(fn) -> int:
    """建池子 → 跑 fn(pool) → 收尾。"""
    pool = Pool(Ledger())
    await pool.start()
    try:
        if not pool.slots():
            log("池内没有账号。先执行：astudio2api accounts import")
            return 2
        return await fn(pool)
    finally:
        await pool.aclose()


# ---------------------------------------------------------------------------
# serve
# ---------------------------------------------------------------------------

def cmd_serve(args: argparse.Namespace) -> int:
    import uvicorn
    from .server import app
    host = args.host
    port = args.port
    log("astudio2api v%s 监听 http://%s:%d", __version__, host, port)
    uvicorn.run(app, host=host, port=port, log_level=args.log_level, access_log=False)
    return 0


# ---------------------------------------------------------------------------
# accounts
# ---------------------------------------------------------------------------

def cmd_accounts(args: argparse.Namespace) -> int:
    action = args.action
    if action == "list":
        accs, errs = acc_mod.load_all()
        if not accs:
            log("accounts/ 下没有账号。用 `astudio2api accounts import` 导入当前客户端登录的账号。")
        for a in accs:
            s = a.summary()
            log("  %-16s uid=%-10s mobile=%sloginMethod=%s banned=%s",
                s["name"], s["uid"], (s["mobile"] + "  ") if s["mobile"] != "***" else "",
                s["loginMethod"], s["banned"])
        for e in errs:
            log("  [警告] %s", e)
        log("共 %d 个账号，目录 %s", len(accs), ACCOUNTS_DIR)
        return 0

    if action == "import":
        try:
            if args.src:
                acc = acc_mod.add_account_file(Path(args.src), args.name, overwrite=args.overwrite)
            else:
                acc = acc_mod.import_live_session(args.name, overwrite=args.overwrite)
        except (FileNotFoundError, FileExistsError, ValueError) as e:
            log("导入失败：%s", e)
            return 1
        log("已导入 %s → %s", acc.name, acc.path)
        return 0

    if action == "remove":
        if not args.name:
            log("需要 --name 指定要移除的账号")
            return 1
        ok = acc_mod.remove_account(args.name)
        log("已移除 %s" % args.name if ok else "账号不存在：%s" % args.name)
        return 0 if ok else 1

    log("未知子命令: %s", action)
    return 1


# ---------------------------------------------------------------------------
# models / credits / checkin / sync
# ---------------------------------------------------------------------------

def cmd_models(args: argparse.Namespace) -> int:
    async def run(pool: Pool) -> int:
        await pool.sync_all(models=True, credits=False)
        for s in pool.slots():
            log("── %s (%s) ──", s.name, s.account.uid or s.identity)
            ms = pool._usable_models(s)  # noqa: SLF001
            for m in ms:
                free = " [免费]" if multiplier_is_free(m.get("multiplier")) else ""
                log("   %-24s %-22s %s%s", m["id"], m.get("name", ""),
                    m.get("multiplier") or "-", free)
            log("   共 %d 个", len(ms))
        return 0
    return asyncio.run(_with_pool(run))


def cmd_credits(args: argparse.Namespace) -> int:
    async def run(pool: Pool) -> int:
        await pool.sync_all(models=False, credits=True)
        for s in pool.slots():
            c = pool.ledger.credits_of(s.identity)
            ck = pool.ledger.entry(s.identity).get("checkin") or {}
            log("── %s ──", s.name)
            log("   合计 %s（%d 段）", c.get("total"), len(c.get("segments") or []))
            for seg in c.get("segments") or []:
                log("     %-10s 剩余 %-10s 到期 %s",
                    seg.get("source"), seg.get("remaining"), _date(seg.get("expires_at")))
            if ck:
                log("   签到 %s → %s（领取 %s 项）", ck.get("date"),
                    "成功" if ck.get("ok") else "失败", ck.get("claimed"))
            err = pool.ledger.entry(s.identity).get("error")
            if err:
                log("   [错误] %s", err)
        return 0
    return asyncio.run(_with_pool(run))


def cmd_checkin(args: argparse.Namespace) -> int:
    async def run(pool: Pool) -> int:
        res = await pool.checkin_all(dry=args.dry, force=args.force)
        bad = 0
        for r in res:
            if r.get("skipped"):
                log("  %-16s 跳过（%s）", r["name"], r["skipped"])
            elif r.get("error"):
                log("  %-16s 失败：%s", r["name"], r["error"])
                bad += 1
            else:
                from astudio.upstream import Upstream
                bad_note = ""
                if not Upstream.signin_succeeded({
                        "claimed": r.get("detail") or [], "errors": r.get("errors") or []}):
                    bad_note = "  ⚠ 未确认签到成功（不占当日名额，下次会重试）"
                gain = f"{_n(r.get('gained')):+g}"
                if r.get("spark_gained"):
                    gain += f"，星火 {r['spark_gained']:+g}"
                left = ""
                if r.get("balance") is not None:
                    left = f"  可用 {r['balance']:g}"
                    if r.get("spark_balance"):
                        left += f" + 星火 {r['spark_balance']:g}"
                log("  %-16s 领取 %s 项，积分 %s%s%s",
                    r["name"], r.get("claimed"), gain, left, bad_note)
                for d in r.get("detail") or []:
                    log("      · %s popupId=%s code=%s",
                        d.get("type"), d.get("popupId"), d.get("code"))
                for e in r.get("errors") or []:
                    log("      ! %s", e)
        return 1 if bad else 0
    return asyncio.run(_with_pool(run))


def cmd_sync(args: argparse.Namespace) -> int:
    async def run(pool: Pool) -> int:
        res = await pool.sync_all(models=True, credits=True)
        for name, n in res["models"].items():
            log("  %-16s %d 个模型", name, n)
        for e in res["errors"]:
            log("  [错误] %s", e)
        return 1 if res["errors"] else 0
    return asyncio.run(_with_pool(run))


# ---------------------------------------------------------------------------
# doctor
# ---------------------------------------------------------------------------

def cmd_doctor(args: argparse.Namespace) -> int:
    """四条链路自检：账号 → 目录 → 积分 → 签到 → 真实推理。"""
    ok = True

    log("① 账号文件")
    accs, errs = acc_mod.load_all()
    if not accs:
        log("   ✗ accounts/ 下没有账号。先在 AStudio 客户端登录，再 `accounts import`")
        return 2
    for a in accs:
        log("   ✓ %s uid=%s", a.name, a.uid or a.identity)
    for e in errs:
        log("   ! %s", e)

    async def run(pool: Pool) -> int:
        nonlocal ok
        log("② 模型目录（%d 个账号）", len(pool.slots()))
        for s in pool.slots():
            try:
                ms = await pool.refresh_models(s)
                free = sum(1 for m in ms if multiplier_is_free(m.get("multiplier")))
                log("   ✓ %s：%d 个模型（%d 个免费）", s.name, len(ms), free)
            except AuthExpired:
                log("   ✗ %s：登录失效，请在 AStudio 客户端重新登录", s.name)
                ok = False
            except Exception as e:  # noqa: BLE001
                log("   ✗ %s：%s", s.name, e)
                ok = False

        log("③ 积分")
        from .ledger import soonest_expiry
        for s in pool.slots():
            try:
                res = await pool.refresh_credits(s)
                segs = res.get("segments") or []
                log("   ✓ %s：合计 %s（%d 段，最早到期 %s）", s.name,
                    res.get("total"), len(segs),
                    _date(soonest_expiry(segs)))
            except Exception as e:  # noqa: BLE001
                log("   ✗ %s：%s", s.name, e)
                ok = False

        log("④ 签到（dry-run，只看待领项不实际领取）")
        for s in pool.slots():
            try:
                r = await s.up.checkin_once(dry=True)
                n = len(r.get("popups") or [])
                log("   ✓ %s：待领 %d 项%s", s.name, n,
                    "" if not r.get("errors") else f"（{r['errors'][0]}）")
            except Exception as e:  # noqa: BLE001
                log("   ✗ %s：%s", s.name, e)
                ok = False

        log("⑤ 真实推理（挑一个账号的一个模型）")
        slot = next((s for s in pool.slots() if s.healthy()), None)
        if slot is None:
            log("   ✗ 没有健康账号")
            return 1
        model = _pick_probe_model(pool, slot)
        try:
            r = await slot.up.chat(
                {"model": model,
                 "messages": [{"role": "user", "content": "Reply with exactly: ok"}],
                 "max_tokens": 256, "stream": False}, False)
            if r.status_code == 200:
                data = r.json()
                msg = (data.get("choices") or [{}])[0].get("message", {}) or {}
                txt = msg.get("content") or msg.get("reasoning_content") or ""
                if not txt:
                    log("   ! %s / %s → 200 但内容为空（推理型模型可能耗尽了 max_tokens）", slot.name, model)
                else:
                    log("   ✓ %s / %s → %r", slot.name, model, str(txt)[:60])
            else:
                log("   ✗ %s / %s → HTTP %s %s", slot.name, model, r.status_code, r.text[:200])
                ok = False
        except Exception as e:  # noqa: BLE001
            log("   ✗ %s / %s → %s", slot.name, model, e)
            ok = False

        log("")
        log("结论：%s", "全部通过 ✓" if ok else "有问题 ✗（见上面 ✗ 行）")
        return 0 if ok else 1

    return asyncio.run(_with_pool(run))


def _pick_probe_model(pool: Pool, slot) -> str:
    """挑一个该账号目录里走 MaaS 网关的模型做冒烟。

    避开 auto（自动路由）与 spark-x（走讯飞原生端点，响应结构不同）。
    """
    skip = {"astronclaw-auto", "spark-x"}
    for m in pool._usable_models(slot):  # noqa: SLF001
        mid = m["id"]
        if mid.lower() not in skip and "spark-api-open" not in str(m.get("base_url") or ""):
            return mid
    # 一个都挑不出来时退到标准集里的第一个，而不是 auto —— auto 已不在对外
    # 清单里，拿它冒烟会把「模型表为空」这个真问题盖掉。
    return BUILTIN_MODELS[0]["id"]


# ---------------------------------------------------------------------------
# 参数解析
# ---------------------------------------------------------------------------

def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="astudio2api",
        description="把讯飞 AStudio 客户端账号池变成 OpenAI 兼容 API",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "示例:\n"
            "  astudio2api accounts import            # 导入客户端当前登录的账号\n"
            "  astudio2api accounts import --name zh2 # 再登一个号后导入为 zh2\n"
            "  astudio2api doctor                     # 四条链路自检\n"
            "  astudio2api serve                      # 起网关\n"
        ))
    p.add_argument("-V", "--version", action="version", version=f"astudio2api {__version__}")
    sub = p.add_subparsers(dest="cmd", required=True)

    s = sub.add_parser("serve", help="启动 OpenAI 兼容网关")
    s.add_argument("--host", default="127.0.0.1")
    s.add_argument("--port", type=int, default=int(__import__("os").environ.get("ASTUDIO_PORT") or 8788))
    s.add_argument("--log-level", default="info")
    s.set_defaults(func=cmd_serve)

    s = sub.add_parser("accounts", help="账号池管理")
    s.add_argument("action", choices=["list", "import", "remove"])
    s.add_argument("--name", help="账号名（导入时可指定，默认按手机号尾号生成）")
    s.add_argument("--src", help="要导入的会话文件路径（省略则用客户端活会话）")
    s.add_argument("--overwrite", action="store_true", help="同名账号已存在时覆盖")
    s.set_defaults(func=cmd_accounts)

    s = sub.add_parser("models", help="列出各账号可用的模型")
    s.set_defaults(func=cmd_models)

    s = sub.add_parser("credits", help="查各账号积分与到期")
    s.set_defaults(func=cmd_credits)

    s = sub.add_parser("checkin", help="执行一轮签到/领奖")
    s.add_argument("--dry", action="store_true", help="只看待领项，不实际领取")
    s.add_argument("--force", action="store_true", help="忽略当日幂等，强制再跑")
    s.set_defaults(func=cmd_checkin)

    s = sub.add_parser("sync", help="刷新全部账号的模型目录与积分")
    s.set_defaults(func=cmd_sync)

    s = sub.add_parser("doctor", help="自检：账号/目录/积分/签到/推理")
    s.set_defaults(func=cmd_doctor)

    return p


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return int(args.func(args) or 0)
    except KeyboardInterrupt:
        log("已中断")
        return 130
