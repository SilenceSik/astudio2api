#!/usr/bin/env python3
"""从本机浏览器读取讯飞（xfyun）登录 cookie。

只取 xfyun 相关域，其它站点的 cookie 一律不读、不打印。
用法：
    python browser_cookie_xfyun.py            # 列出所有 profile 里的讯飞 cookie（值打码）
    python browser_cookie_xfyun.py --show     # 显示完整值（仅本机排障用）
"""
from __future__ import annotations

import base64
import json
import os
import shutil
import sqlite3
import sys
import tempfile
from pathlib import Path

import win32crypt
from Cryptodome.Cipher import AES

LA = os.environ["LOCALAPPDATA"]
BROWSERS = {
    "Chrome": Path(LA) / r"Google\Chrome\User Data",
    "Edge": Path(LA) / r"Microsoft\Edge\User Data",
    "Chromium": Path(LA) / r"Chromium\User Data",
    "Brave": Path(LA) / r"BraveSoftware\Brave-Browser\User Data",
}

# 只认讯飞域
WANT_SUFFIXES = ("xfyun.cn", "xfyun.com")


def aes_key_for(base: Path):
    """从 Local State 取 DPAPI 保护的 AES key。"""
    ls = base / "Local State"
    if not ls.exists():
        return None
    blob = json.loads(ls.read_text(encoding="utf-8"))["os_crypt"]["encrypted_key"]
    raw = base64.b64decode(blob)
    if raw[:5] != b"DPAPI":
        return None
    return win32crypt.CryptUnprotectData(raw[5:], None, None, None, 0)[1]


def decrypt(value: bytes, key: bytes) -> str | None:
    if not value:
        return None
    # 新格式：v10/v11 前缀 + AES-GCM
    if value[:3] in (b"v10", b"v11") and key:
        try:
            nonce, body = value[3:15], value[15:]
            cipher = AES.new(key, AES.MODE_GCM, nonce=nonce)
            return cipher.decrypt_and_verify(body[:-16], body[-16:]).decode("utf-8", "replace")
        except Exception:
            return None
    # 老格式：整块 DPAPI
    try:
        return win32crypt.CryptUnprotectData(value, None, None, None, 0)[1].decode("utf-8", "replace")
    except Exception:
        return None


def profiles(base: Path):
    if not base.exists():
        return
    for prof in sorted(base.iterdir()):
        if not prof.is_dir():
            continue
        for sub in ("Network/Cookies", "Cookies"):
            if (prof / sub).exists():
                yield prof.name, prof / sub


def interesting(host: str) -> bool:
    h = host.lstrip(".").lower()
    return any(h == s or h.endswith("." + s) for s in WANT_SUFFIXES)


def main() -> int:
    show = "--show" in sys.argv
    total = 0
    for bname, base in BROWSERS.items():
        if not base.exists():
            continue
        key = aes_key_for(base)
        for pname, db in profiles(base):
            # 运行中的浏览器会锁库，先复制一份再读
            tmp = Path(tempfile.gettempdir()) / f"ck_{bname}_{pname}.db"
            try:
                shutil.copy2(db, tmp)
            except Exception as e:  # noqa: BLE001
                print(f"  ✗ {bname}/{pname} 复制失败: {e}")
                continue
            else:
                for wal in ("-wal", "-shm"):
                    src = Path(str(db) + wal)
                    if src.exists():
                        try:
                            shutil.copy2(src, Path(str(tmp) + wal))
                        except Exception:  # noqa: BLE001
                            pass
            try:
                con = sqlite3.connect(f"file:{tmp}?mode=ro", uri=True)
                rows = con.execute(
                    "SELECT host_key, name, encrypted_value, path, expires_utc "
                    "FROM cookies"
                ).fetchall()
                con.close()
            except Exception as e:  # noqa: BLE001
                print(f"  ✗ {bname}/{pname} 读取失败: {e}")
                continue
            finally:
                try:
                    tmp.unlink()
                except Exception:  # noqa: BLE001
                    pass

            hits = []
            for host, name, enc, path, exp in rows:
                if not interesting(host or ""):
                    continue
                val = decrypt(enc, key)
                if val:
                    hits.append((host, name, val, path))
            if hits:
                print(f"\n=== {bname} / {pname} （共 {len(rows)} 条 cookie，讯飞相关 {len(hits)} 条）===")
                for host, name, val, path in sorted(hits):
                    shown = val if show else (val[:4] + "…" + val[-2:] if len(val) > 8 else "***")
                    print(f"  {host:28} {name:24} = {shown}")
                total += len(hits)
    if total == 0:
        print("\n本机浏览器里没有讯飞相关 cookie（还没在这些浏览器登录过讯飞）。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
