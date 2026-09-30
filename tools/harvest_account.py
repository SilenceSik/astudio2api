#!/usr/bin/env python3
"""从本机浏览器抓取讯飞登录凭证 → 验活 → 落盘为账号文件。

两条取证路线（都跑，按 sid 去重）：

  A. 浏览历史（首选）
     Chrome 运行时对 Network/Cookies 是**独占锁**，共享读也拿不到；History 不锁。
     讯飞登录成功会跳转到
         https://sso.xfyun.cn/SSOService/login/setcookies?ssoSessionId=<明文>&account_id=<明文>
     这个 URL 原样记进历史 —— ssoSessionId 是明文。
     注意：**关掉浏览器后** Chrome 才会把 cookie 落盘；历史则是即时写入的。

  B. cookie 库（浏览器没开时可用）
     解密 Chrome 系 cookie（DPAPI 取 key + AES-GCM），只取 xfyun 域。
     本机 Chrome 的 user-data-dir 不是默认路径，见 UDD_ROOTS。

用法：
    python tools/harvest_account.py                # 列出找到的凭证并验活
    python tools/harvest_account.py --save         # 验活通过后落盘到 accounts/
    python tools/harvest_account.py --save --name acct-XXXX
"""
from __future__ import annotations

import base64
import ctypes
import ctypes.wintypes as wt
import datetime as _dt
import json
import os
import re
import shutil
import sqlite3
import sys
import tempfile
import urllib.request
from pathlib import Path

import win32crypt
from Cryptodome.Cipher import AES

BASE = Path(__file__).resolve().parent.parent

PROBE = "https://agent.xfyun.cn/xingchen-studio/bot/models/configs"
H = {"Accept": "application/json", "User-Agent": "AStudio/3.4.4",
     "clientType": "21", "studioVersion": "3.4.4"}

# 浏览器 user-data 根目录。
# 除各家默认位置外，可用 ASTUDIO_BROWSER_DATA 追加自定义根目录（分号或冒号分隔）
# —— 装在非默认盘的浏览器用这个补充，
# 不必改代码。
UDD_ROOTS = [
    Path(os.environ.get("LOCALAPPDATA", "")) / "Google/Chrome/User Data",
    Path(os.environ.get("LOCALAPPDATA", "")) / "Microsoft/Edge/User Data",
    Path(os.environ.get("LOCALAPPDATA", "")) / "Chromium/User Data",
    Path(os.environ.get("LOCALAPPDATA", "")) / "BraveSoftware/Brave-Browser/User Data",
]
for _extra in re.split(r"[;:]", os.environ.get("ASTUDIO_BROWSER_DATA", "")):
    if _extra.strip():
        UDD_ROOTS.append(Path(_extra.strip()))
        UDD_ROOTS.append(Path(_extra.strip()) / "User Data")

_SID_RE = re.compile(r"ssoSessionId=([0-9a-fA-F]{8}-[0-9a-fA-F-]{27,})")
_ACCT_RE = re.compile(r"[?&]account_id=(\w+)")


def _chrome_ts(v: int) -> str:
    if not v or v < 10_000_000:
        return "会话"
    d = _dt.datetime(1601, 1, 1) + _dt.timedelta(microseconds=v) + _dt.timedelta(hours=8)
    return d.strftime("%m-%d %H:%M")


def _copy_locked(src: Path, dst: Path) -> int:
    """共享读方式复制被占用的文件。返回字节数；-1 = 被独占锁挡下。"""
    GENERIC_READ, SHARE_ALL, OPEN_EXISTING = 0x80000000, 0x7, 3
    k32 = ctypes.WinDLL("kernel32", use_last_error=True)
    k32.CreateFileW.restype = ctypes.c_void_p
    k32.CreateFileW.argtypes = [wt.LPCWSTR, wt.DWORD, wt.DWORD, ctypes.c_void_p,
                                wt.DWORD, wt.DWORD, ctypes.c_void_p]
    k32.ReadFile.argtypes = [ctypes.c_void_p, ctypes.c_void_p, wt.DWORD,
                             ctypes.POINTER(wt.DWORD), ctypes.c_void_p]
    k32.CloseHandle.argtypes = [ctypes.c_void_p]
    h = k32.CreateFileW(str(src), GENERIC_READ, SHARE_ALL, None, OPEN_EXISTING, 0, None)
    if h in (ctypes.c_void_p(-1).value, None):
        return -1
    buf, n, out = ctypes.create_string_buffer(1 << 20), wt.DWORD(0), bytearray()
    try:
        while k32.ReadFile(h, buf, len(buf), ctypes.byref(n), None) and n.value:
            out += buf.raw[: n.value]
    finally:
        k32.CloseHandle(h)
    dst.write_bytes(bytes(out))
    return len(out)


def _snapshot(db: Path) -> Path | None:
    """给 sqlite 库做可读快照（含 -wal/-shm）。被独占锁挡住时返回 None。"""
    tmp = Path(tempfile.gettempdir()) / f"hv_{abs(hash(str(db)))}.db"
    if _copy_locked(db, tmp) < 0:
        try:
            shutil.copy2(db, tmp)
        except Exception:
            return None
    for suf in ("-wal", "-shm"):
        s = Path(str(db) + suf)
        if s.exists():
            if _copy_locked(s, Path(str(tmp) + suf)) < 0:
                try:
                    shutil.copy2(s, Path(str(tmp) + suf))
                except Exception:
                    pass
    return tmp


def _cleanup(tmp: Path) -> None:
    for f in (tmp, Path(str(tmp) + "-wal"), Path(str(tmp) + "-shm")):
        try:
            f.unlink()
        except Exception:
            pass


def _aes_key(udd: Path) -> bytes | None:
    for ls in (udd / "Local State", udd / "User Data" / "Local State"):
        if not ls.exists():
            continue
        try:
            blob = json.loads(ls.read_text(encoding="utf-8"))["os_crypt"]["encrypted_key"]
            raw = base64.b64decode(blob)
            if raw[:5] == b"DPAPI":
                return win32crypt.CryptUnprotectData(raw[5:], None, None, None, 0)[1]
        except Exception:
            continue
    return None


def _decrypt(v: bytes, key: bytes | None) -> str | None:
    if not v:
        return None
    if v[:3] in (b"v10", b"v11") and key:
        try:
            return AES.new(key, AES.MODE_GCM, nonce=v[3:15]) \
                .decrypt_and_verify(v[15:-16], v[-16:]).decode("utf-8", "replace")
        except Exception:
            return None
    try:
        return win32crypt.CryptUnprotectData(v, None, None, None, 0)[1].decode("utf-8", "replace")
    except Exception:
        return None


def from_history() -> list[dict]:
    """路线 A：浏览历史里的 setcookies URL（Chrome 开着也能读）。"""
    out: list[dict] = []
    for udd in UDD_ROOTS:
        if not udd.is_dir():
            continue
        for hist in udd.glob("*/History"):
            tmp = _snapshot(hist)
            if not tmp:
                continue
            try:
                con = sqlite3.connect(f"file:{tmp}?mode=ro", uri=True)
                rows = con.execute(
                    "SELECT url,last_visit_time FROM urls WHERE url LIKE '%ssoSessionId=%' "
                    "ORDER BY last_visit_time DESC LIMIT 10").fetchall()
                con.close()
            except Exception:
                continue
            finally:
                _cleanup(tmp)
            seen: set[str] = set()
            for url, vt in rows:
                m = _SID_RE.search(url)
                if not m:
                    continue
                sid = m.group(1)
                if sid in seen:
                    continue
                seen.add(sid)
                a = _ACCT_RE.search(url)
                out.append({"src": f"{hist.parent.parent.name}/{hist.parent.name}·历史",
                            "ssoSessionId": sid,
                            "account_id": a.group(1) if a else "",
                            "when": _chrome_ts(vt)})
    return out


def from_cookies() -> list[dict]:
    """路线 B：cookie 库（浏览器没开时最准）。"""
    out: list[dict] = []
    for udd in UDD_ROOTS:
        if not udd.is_dir():
            continue
        key = _aes_key(udd)
        for db in list(udd.glob("*/Network/Cookies")) + list(udd.glob("*/Cookies")):
            tmp = _snapshot(db)
            if not tmp:
                continue
            try:
                con = sqlite3.connect(f"file:{tmp}?mode=ro", uri=True)
                rows = con.execute(
                    "SELECT host_key,name,encrypted_value FROM cookies").fetchall()
                con.close()
            except Exception:
                continue
            finally:
                _cleanup(tmp)
            jar: dict[str, str] = {}
            for host, name, enc in rows:
                h = (host or "").lstrip(".").lower()
                if not (h == "xfyun.cn" or h.endswith(".xfyun.cn")):
                    continue
                if name not in ("ssoSessionId", "sso_sessionid", "account_id"):
                    continue
                val = _decrypt(enc, key)
                if val and name not in jar:
                    jar[name] = val
            sid = jar.get("ssoSessionId") or jar.get("sso_sessionid")
            if sid:
                pid = db.parent.parent.name if db.parent.name == "Network" else db.parent.name
                out.append({"src": f"{udd.name}/{pid}·cookie",
                            "ssoSessionId": sid,
                            "account_id": jar.get("account_id", ""),
                            "when": "cookie库"})
    return out


def verify(sid: str) -> tuple[bool, str, str]:
    """验活。返回 (是否可用, 说明, 服务端下发的 bearer)。"""
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
    return True, f"{len(items)} 个模型 / bearer={'有' if bearer else '无'}", bearer


def mask(v: str) -> str:
    return "***" if len(v) <= 8 else v[:4] + "…" + v[-3:]


def main() -> int:
    do_save = "--save" in sys.argv
    name = ""
    if "--name" in sys.argv:
        i = sys.argv.index("--name")
        if i + 1 < len(sys.argv):
            name = sys.argv[i + 1].strip()

    print("① 取证：浏览历史")
    hits = from_history()
    print(f"   {len(hits)} 条")
    print("② 取证：cookie 库")
    heard = {h["ssoSessionId"] for h in hits}
    extra = [h for h in from_cookies() if h["ssoSessionId"] not in heard]
    for h in extra:
        print(f"   + {mask(h['ssoSessionId'])} acct={h['account_id'] or '-'}  {h['src']}")
    hits += extra

    if not hits:
        print("\n✗ 没找到任何讯飞凭证。")
        print("  请先在浏览器打开 https://passport.xfyun.cn/ 用目标账号登录。")
        print("  （Chrome 的 cookie 要等浏览器关闭才落盘；历史记录是即时的。）")
        return 2

    uniq: dict[str, dict] = {}
    for h in hits:
        uniq.setdefault(h["ssoSessionId"], h)
    print(f"\n③ 去重后 {len(uniq)} 个会话，逐个验活")
    good = []
    for i, (sid, h) in enumerate(uniq.items(), 1):
        ok, msg, bearer = verify(sid)
        print(f"   {'✅' if ok else '❌'} #{i} sid={mask(sid)} acct={h['account_id'] or '-':>12} "
              f"{h['when']:>10}  → {msg}")
        if ok:
            good.append((h, sid, bearer))

    if not good:
        print("\n没有可用凭证（可能都过期了，重新登录一次）。")
        return 1
    print(f"\n④ {len(good)} 个可用")
    if not do_save:
        print("   （加 --save 落盘到 accounts/）")
        return 0

    for h, sid, bearer in good:
        nm = name or (f"acct-{h['account_id'][-4:]}" if h["account_id"] else f"acct-{sid[:4]}")
        dest = BASE / "accounts" / f"{nm}.json"
        if dest.exists() and not name:
            print(f"   · {nm} 已存在，跳过（覆盖请用 --name {nm}）")
            continue
        dest.parent.mkdir(parents=True, exist_ok=True)
        # modelBearerToken 是池子的准入条件（load_all 会过滤掉缺它的账号），
        # 必须用服务端下发的 api_key，不能留空或占位符。
        payload = {
            "accountId": h["account_id"] or sid,
            "uid": h["account_id"] or sid,
            "token": sid,
            "ssoSessionId": sid,
            "modelBearerToken": bearer,
            "banned": False,
            "loginMethod": "password",
            "loggedInAt": _dt.datetime.now(_dt.timezone.utc)
                            .strftime("%Y-%m-%dT%H:%M:%S.000Z"),
            "_source": h["src"],
        }
        dest.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
        try:
            os.chmod(dest, 0o600)
        except OSError:
            pass
        print(f"   ✓ 落盘 {dest.name}  (sid={mask(sid)})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
