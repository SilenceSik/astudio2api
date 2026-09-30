#!/usr/bin/env python3
"""探测讯飞微信扫码登录接口，看返回什么。

流程（逆向自 AStudio 客户端）：
    qrcode?fd=<8位>  → 生成二维码
    uids?fd=<8位>    → 轮询；扫码后返回账号列表，含 uid + ssoSessionId
"""
import json
import secrets
import urllib.error
import urllib.parse
import urllib.request

BASE = "https://sso.xfyun.cn/"
ALPHABET = "ABCDEFGHJKMNPQRSTWXYZabcdefhijkmnprstwxyz2345678"
HEADERS = {
    "Accept": "application/json",
    "X-Requested-With": "XMLHttpRequest",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AStudio/3.4.4",
}


def new_fd() -> str:
    return "".join(secrets.choice(ALPHABET) for _ in range(8))


def call(path: str, params: dict | None = None) -> tuple[int, str]:
    url = BASE + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, headers=HEADERS)
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001
        return -1, f"{type(e).__name__}: {e}"


fd = new_fd()
print(f"生成 fd = {fd}\n")

for path, params in [
    ("SSOService/wx/scanlogin/qrcode", {"fd": fd}),
    ("SSOService/wx/scanlogin/uids", {"fd": fd}),
]:
    code, body = call(path, params)
    print(f"── {path}  → HTTP {code}")
    try:
        d = json.loads(body)
        print("   ", json.dumps(d, ensure_ascii=False)[:700])
    except Exception:  # noqa: BLE001
        print("    (非 JSON)", body[:300].replace("\n", " "))
    print()
