"""账号（会话凭据）的加载、导入与身份识别。

每个账号一个 JSON 文件，放在 `accounts/` 下，文件名即账号名。
内容就是 AStudio 客户端的 `astron-session.json`（多账号时各自存一份）。
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .consts import ACCOUNTS_DIR, CLIENT_TYPE_WIN, STUDIO_VERSION, SESSION_CANDIDATES, log


def _mask(v: Any) -> str:
    s = str(v or "")
    return f"{s[:4]}…{s[-2:]}" if len(s) > 8 else "***"


@dataclass
class Account:
    """一个 AStudio 账号凭据。"""

    name: str            # 文件名去掉后缀，用作账号标识
    path: Path
    raw: dict[str, Any] = field(default_factory=dict)

    # -- 身份 --------------------------------------------------------------
    @property
    def account_id(self) -> str:
        return str(self.raw.get("accountId") or "")

    @property
    def uid(self) -> str:
        return str(self.raw.get("uid") or "")

    @property
    def bearer(self) -> str:
        return str(self.raw.get("modelBearerToken") or "").strip()

    @property
    def banned(self) -> bool:
        return self.raw.get("banned") is True

    @property
    def mobile(self) -> str:
        return str(self.raw.get("mobile") or "")

    @property
    def identity(self) -> str:
        """稳定身份指纹：用于台账绑定、防止换号后旧余额串到新号。"""
        seed = f"{self.account_id}|{self.uid}|{self.mobile}"
        return hashlib.sha256(seed.encode("utf-8")).hexdigest()[:16]

    # -- 请求头 ------------------------------------------------------------
    def cookie(self) -> str:
        parts: list[str] = []
        sid = self.raw.get("ssoSessionId")
        if sid:
            parts.append(f"ssoSessionId={sid}")
            parts.append(f"sso_sessionid={sid}")
        parts.append(f"account_id={self.account_id}")
        parts.append(f"token={self.raw.get('token')}")
        return "; ".join(parts)

    def studio_headers(self) -> dict[str, str]:
        return {
            "Accept": "application/json",
            "User-Agent": f"AStudio/{STUDIO_VERSION}",
            "clientType": CLIENT_TYPE_WIN,
            "studioVersion": STUDIO_VERSION,
            "Cookie": self.cookie(),
        }

    def maas_headers(self, key: str | None = None) -> dict[str, str]:
        return {
            "Accept": "application/json",
            "User-Agent": f"AStudio/{STUDIO_VERSION}",
            "Authorization": f"Bearer {key or self.bearer}",
            "uid": self.uid,
        }

    # -- 落盘 --------------------------------------------------------------
    def save(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self.path.with_suffix(self.path.suffix + ".tmp")
        tmp.write_text(json.dumps(self.raw, ensure_ascii=False, indent=2), encoding="utf-8")
        os.replace(tmp, self.path)
        try:
            os.chmod(self.path, 0o600)
        except OSError:
            pass

    def summary(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "path": str(self.path),
            "identity": self.identity,
            "accountId": _mask(self.account_id),
            "uid": self.uid or _mask(self.account_id),
            "bearer": _mask(self.bearer),
            "mobile": _mask(self.mobile),
            "loginMethod": self.raw.get("loginMethod"),
            "loggedInAt": self.raw.get("loggedInAt"),
            "banned": self.banned,
        }


def account_files() -> list[Path]:
    """列出 accounts/ 下的账号文件（*.json，忽略隐藏与临时文件）。"""
    d = ACCOUNTS_DIR
    if not d.is_dir():
        return []
    out: list[Path] = []
    for p in sorted(d.glob("*.json")):
        if p.name.startswith(".") or p.name.endswith(".tmp"):
            continue
        if p.name in {"state.json", "ledger.json", "checkin.json"}:
            continue
        out.append(p)
    return out


def load_account_file(p: Path) -> Account:
    with p.open(encoding="utf-8") as f:
        raw = json.load(f)
    if not isinstance(raw, dict):
        raise ValueError(f"{p} 内容不是 JSON 对象")
    return Account(name=p.stem, path=p, raw=raw)


def load_all() -> tuple[list[Account], list[str]]:
    """加载全部账号；返回 (成功列表, 错误列表)。单个文件坏掉不影响其余。"""
    ok: list[Account] = []
    errs: list[str] = []
    for p in account_files():
        try:
            acc = load_account_file(p)
            if not acc.bearer:
                errs.append(f"{p.name}: 缺少 modelBearerToken，跳过")
                continue
            ok.append(acc)
        except Exception as e:  # noqa: BLE001
            errs.append(f"{p.name}: {e}")
    return ok, errs


def find_live_session() -> Path | None:
    """找 AStudio 客户端当前写入的活会话文件。"""
    for c in SESSION_CANDIDATES:
        if c.exists():
            return c
    return None


def import_live_session(name: str | None = None, *, overwrite: bool = False) -> Account:
    """把客户端活会话复制进 accounts/，成为池里的一员。"""
    src = find_live_session()
    if src is None:
        raise FileNotFoundError(
            "找不到 AStudio 客户端会话文件；请先在客户端登录，"
            "或用 `astudio2api accounts add <文件路径> --name <名称>` 手动导入。"
        )
    with src.open(encoding="utf-8") as f:
        raw = json.load(f)
    acc = Account(name=name or src.parent.parent.name or "default", path=src, raw=raw)
    if not name:
        # 默认用手机号后四位/账号标识命名，便于区分多号
        tail = acc.mobile[-4:] if len(acc.mobile) >= 4 else acc.identity[:4]
        acc.name = f"acct-{tail}"
    dest = ACCOUNTS_DIR / f"{acc.name}.json"
    if dest.exists() and not overwrite:
        raise FileExistsError(f"账号 {acc.name} 已存在（加 --overwrite 覆盖）")
    acc.path = dest
    acc.save()
    log("已导入账号 %s (uid=%s)", acc.name, acc.uid or _mask(acc.account_id))
    return acc


def add_account_file(src: Path, name: str | None = None, *, overwrite: bool = False) -> Account:
    """从任意路径导入一份会话文件。"""
    if not src.exists():
        raise FileNotFoundError(f"文件不存在: {src}")
    acc = load_account_file(src)
    acc.name = name or src.stem
    dest = ACCOUNTS_DIR / f"{acc.name}.json"
    if dest.exists() and not overwrite:
        raise FileExistsError(f"账号 {acc.name} 已存在（加 --overwrite 覆盖）")
    dest.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src, dest)
    acc.path = dest
    log("已导入账号 %s (uid=%s)", acc.name, acc.uid or _mask(acc.account_id))
    return acc


def remove_account(name: str) -> bool:
    dest = ACCOUNTS_DIR / f"{name}.json"
    if not dest.exists():
        return False
    dest.unlink()
    return True
