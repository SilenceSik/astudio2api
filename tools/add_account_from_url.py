#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""从浏览器地址栏 URL 一键落地账号，可选推到远端网关。

在 https://passport.xfyun.cn/ 登录完成后，地址栏会出现：

    https://www.xfyun.cn/?ssoSessionId=<uuid>&account_id=<digits>

把这一串整段丢给本工具，它会：
  1. 解析 ssoSessionId / account_id
  2. 打上游验活，并取服务端下发的 modelBearerToken（凭证准入条件）
  3. 写 accounts/acct-<尾4>.json（本地留存一份）
  4. 若设了 ASTUDIO_SSH_HOST，再 scp 到远端账号目录
  5. 远程热加载 + 健康检查，回报池子状态

全程不读浏览器数据。用法：

    python tools/add_account_from_url.py "<地址栏URL>"
    python tools/add_account_from_url.py            # 交互式粘贴
"""
from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
ACCOUNTS = ROOT / "accounts"

# 远端推送（可选）。不设 ASTUDIO_SSH_HOST 就只写本地账号目录，不碰远端。
# 这些值全部来自环境，仓库里不留任何主机名、路径或密钥位置。
HOST = os.environ.get("ASTUDIO_SSH_HOST", "")
KEY = Path(os.environ["ASTUDIO_SSH_KEY"]) if os.environ.get("ASTUDIO_SSH_KEY") else None
RDIR = os.environ.get("ASTUDIO_REMOTE_DIR", "/opt/astudio2api")
RSVC = os.environ.get("ASTUDIO_REMOTE_SERVICE", "astudio2api")

PROBE = "https://agent.xfyun.cn/xingchen-studio/bot/models/configs"
H = {"Accept": "application/json", "User-Agent": "AStudio/3.4.4",
     "clientType": "21", "studioVersion": "3.4.4"}

_SID_RE = re.compile(r"ssoSessionId=([0-9a-fA-F]{8}-[0-9a-fA-F-]{27,})")
_ACCT_RE = re.compile(r"account_id=(\d{6,20})")
_BARE_SID_RE = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F-]{27,}$")


def mask(v: str) -> str:
    return "***" if len(v) <= 8 else v[:4] + "…" + v[-4:]


def parse(text: str) -> tuple[str, str]:
    """从一段文本里抠出 (ssoSessionId, account_id)。"""
    text = text.strip().strip('"').strip("'")
    m_sid = _SID_RE.search(text)
    m_acct = _ACCT_RE.search(text)
    if not m_sid and _BARE_SID_RE.match(text):
        raise SystemExit("✗ 只给了 ssoSessionId，缺 account_id。请粘完整地址栏 URL。")
    if not m_sid:
        raise SystemExit("✗ 没找到 ssoSessionId。请确认复制的是登录后地址栏那一整串 URL。")
    if not m_acct:
        raise SystemExit("✗ 没找到 account_id。请粘完整地址栏 URL（含 &account_id=…）。")
    return m_sid.group(1), m_acct.group(1)


def verify(sid: str) -> tuple[bool, str, str]:
    """验活并取服务端下发的 bearer。"""
    req = urllib.request.Request(
        PROBE, headers={**H, "Cookie": f"ssoSessionId={sid}; sso_sessionid={sid}"})
    try:
        with urllib.request.urlopen(req, timeout=25) as r:
            d = json.loads(r.read())
    except Exception as e:  # noqa: BLE001
        return False, f"{type(e).__name__}: {str(e)[:70]}", ""
    items = [i for i in (d.get("data") or []) if isinstance(i, dict)]
    if not items:
        return False, f"code={d.get('code')} desc={str(d.get('desc'))[:60]}", ""
    bearer = next((i["api_key"] for i in items if i.get("api_key")), "")
    return True, f"{len(items)} 个模型", bearer


def _ssh_argv() -> list[str]:
    """共用 ssh 参数。没配 ASTUDIO_SSH_KEY 就走 ssh 默认身份（agent / ~/.ssh）。"""
    argv = ["-F", "/dev/null", "-o", "ConnectTimeout=20",
            "-o", "StrictHostKeyChecking=accept-new"]
    if KEY is not None:
        argv += ["-i", str(KEY)]
    return argv


def ssh(cmd: str, timeout: int = 90) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["ssh", *_ssh_argv(), f"root@{HOST}", cmd],
        capture_output=True, text=True, timeout=timeout)


def scp(local: Path) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["scp", *_ssh_argv(), str(local), f"root@{HOST}:{RDIR}/accounts/"],
        capture_output=True, text=True, timeout=180)


def resolve_name(accounts_dir: Path, acct: str) -> tuple[str, str]:
    """决定账号文件名。返回 (最终文件名, 说明)。

    规则：默认 `acct-<accountId 前4位>`（与既有账号文件一致）；
    但若目录里已有**同一 accountId** 的文件，就续用原文件名 —— 同一账号在池子里
    出现两份凭据会让它的调度权重翻倍。
    """
    default = f"acct-{acct[:4]}"
    if not accounts_dir.is_dir():
        return default, ""
    for p in sorted(accounts_dir.glob("*.json")):
        try:
            data = json.loads(p.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            continue
        if data.get("accountId") == acct:
            if p.stem != default:
                return p.stem, f"已存在同账号文件 {p.stem}，续用它（避免重复入池）"
            return p.stem, ""
    return default, ""


def main() -> int:
    if len(sys.argv) > 1:
        raw = " ".join(sys.argv[1:])
    else:
        print("粘贴登录后地址栏那一整串 URL，回车：")
        raw = sys.stdin.readline()
    if not raw.strip():
        raise SystemExit("✗ 空输入。")

    sid, acct = parse(raw)
    print(f"→ 解析到 account_id={acct}  session={mask(sid)}")

    ok, why, bearer = verify(sid)
    if not ok:
        print(f"✗ 验活失败：{why}")
        print("  可能原因：session 已过期 / 复制的是旧 URL / 该账号未开通 AStudio。")
        return 1
    if not bearer:
        print("✗ 验活通过但上游没下发 modelBearerToken —— 账号缺准入凭证，不入池。")
        return 1
    print(f"✓ 验活通过：{why}，bearer={mask(bearer)}")

    ACCOUNTS.mkdir(exist_ok=True)
    name, note = resolve_name(ACCOUNTS, acct)
    if note:
        print(f"→ {note}")

    obj = {
        "accountId": acct,
        "uid": acct,
        "token": sid,
        "ssoSessionId": sid,
        "modelBearerToken": bearer,
        "banned": False,
        "loginMethod": "password",
        "loggedInAt": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.000Z"),
    }
    dst = ACCOUNTS / f"{name}.json"
    existed = dst.exists()
    dst.write_text(json.dumps(obj, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"✓ {'覆盖' if existed else '写入'} {dst.relative_to(ROOT)}")

    if not HOST:
        print("→ 未设 ASTUDIO_SSH_HOST，跳过远端推送（仅写本地账号目录）")
        return 0

    print(f"→ 推送到 {HOST}…")
    cp = scp(dst)
    if cp.returncode != 0:
        print(f"✗ scp 失败：{(cp.stderr or cp.stdout).strip()[:200]}")
        return 1

    print("→ 热加载…")
    cp = ssh(
        f"cd {RDIR} && set -a && . ./.env && set +a && "
        f"curl -s -m 30 -X POST -H \"Authorization: Bearer $ASTUDIO_API_KEY\" "
        f"http://127.0.0.1:8788/admin/reload && echo && "
        f"curl -s -m 20 -H \"Authorization: Bearer $ASTUDIO_API_KEY\" "
        f"http://127.0.0.1:8788/health")
    out = (cp.stdout or "").strip()
    if cp.returncode != 0 or not out:
        print(f"✗ 远程热加载失败：{(cp.stderr or '').strip()[:200]}")
        return 1
    for line in out.splitlines():
        if line.strip():
            print("  " + line.strip())

    print(f"\n✓ 完成。{name} 已入账号池。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
