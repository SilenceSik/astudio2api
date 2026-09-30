"""签到（checkin）语义回归。

实测结论（2026-10-01 对真实上游验证）：
  - 真正发分的是 POST tenant-app/v2/init-app（客户端登录时调的「签到」）
  - client-popups/* 的 claim / complete 都是空操作：连早已领过的账号调 claim
    也返回 flag:true/code:0 且余额不变
  - 旧版 checkin 只调那些空操作接口，一分没拿到却把当天标成「已签到」
    → 当天再不复签 → 账号静默漏签（新号「一直没有积分」的成因）
"""
from __future__ import annotations

import asyncio
from typing import Any

import pytest

from astudio.upstream import Upstream


# ---------------------------------------------------------------------------
# 假的 studio 上游
# ---------------------------------------------------------------------------
class FakeStudio:
    """记录调用路径；init-app 按天幂等发分。"""

    def __init__(self, *, granted_step: int = 100, spark_step: int = 5000,
                 signed: bool = False, banned: bool = False,
                 popups: list[dict[str, Any]] | None = None):
        self.calls: list[str] = []
        self.granted = 1000
        self.avail = 1000
        self.spark = 0
        self.signed = signed
        self.granted_step = granted_step
        self.spark_step = spark_step
        self.banned = banned
        self.popups = popups or []

    def _balance(self) -> dict[str, Any]:
        return {"totalAmount": self.granted, "totalBalance": self.avail,
                "sparkTotalAmount": self.spark, "sparkTotalBalance": self.spark}

    async def handle(self, method: str, path: str, body: Any) -> dict[str, Any]:
        self.calls.append(path)
        if path.endswith("tenant-app/v2/init-app"):
            assert method == "POST", "init-app 必须是 POST"
            if not self.signed:
                self.signed = True
                self.granted += self.granted_step
                self.spark += self.spark_step
            return {"flag": True, "code": 0, "desc": "成功",
                    "data": {"banned": self.banned}}
        if path.endswith("points/balance"):
            return {"flag": True, "code": 0, "desc": "成功", "data": self._balance()}
        if path.endswith("client-popups/pending"):
            return {"flag": True, "code": 0, "desc": "成功", "data": self.popups}
        if path.endswith("client-download-reward/claim"):
            # 空操作：返回成功但一分不发（实测行为）
            return {"flag": True, "code": 0, "desc": "成功", "data": None}
        if path.endswith("client-popups/complete"):
            return {"flag": True, "code": 0, "desc": "成功", "data": None}
        raise AssertionError(f"未预期请求: {method} {path}")


def make_upstream(fake: FakeStudio) -> Upstream:
    up = Upstream.__new__(Upstream)  # 绕过 __init__ 的网络/账号依赖

    async def _get(path: str) -> dict[str, Any]:
        return await fake.handle("GET", path, None)

    async def _post(path: str, body: Any = None) -> dict[str, Any]:
        return await fake.handle("POST", path, body)

    up._studio_get = _get          # type: ignore[method-assign]
    up._studio_post = _post        # type: ignore[method-assign]
    return up


# ---------------------------------------------------------------------------
# 断言
# ---------------------------------------------------------------------------
def test_checkin_calls_init_app():
    """签到必须真的打 init-app —— 那才是发分口。"""
    fake = FakeStudio()
    up = make_upstream(fake)

    res = asyncio.run(up.checkin_once(dry=False))

    assert "tenant-app/v2/init-app" in " ".join(fake.calls), "必须调用 init-app"
    assert res["gained"] == 100, f"应报告 +100，实得 {res['gained']}"
    assert res["spark_gained"] == 5000, f"应报告星火 +5000，实得 {res['spark_gained']}"
    assert any(c["type"] == "SIGN_IN" for c in res["claimed"]), "应留下 SIGN_IN 记录"


def test_dry_run_does_not_sign_in():
    """dry-run 绝不能真的签到。"""
    fake = FakeStudio()
    up = make_upstream(fake)

    asyncio.run(up.checkin_once(dry=True))

    assert not any("init-app" in c for c in fake.calls), "dry-run 不该调 init-app"


def test_gain_uses_granted_not_available():
    """战果按「累计发放」算，不被当天的消耗干扰。

    这里模拟签到同时花掉 400 分：可用余额反而下降，但累计发放 +100。
    """
    fake = FakeStudio(granted_step=100)
    up = make_upstream(fake)

    orig = fake.handle

    async def handle(method: str, path: str, body: Any) -> dict[str, Any]:
        r = await orig(method, path, body)
        if path.endswith("init-app"):
            fake.avail -= 400  # 同时消耗
        return r

    fake.handle = handle  # type: ignore[method-assign]

    res = asyncio.run(up.checkin_once(dry=False))

    assert res["gained"] == 100, (
        f"战果应按累计发放算 +100，实得 {res['gained']}（用 totalBalance 会算成 -300）"
    )
    assert res["balance"] == 600, f"可用余额应单独报告 600，实得 {res['balance']}"


def test_signin_succeeded_requires_init_app_marker():
    """判据是「签到那一步真的成功了」，不是「跑过了」。"""
    up = Upstream
    assert up.signin_succeeded({"claimed": [{"type": "SIGN_IN"}], "errors": []})
    assert not up.signin_succeeded({"claimed": [], "errors": []})
    assert not up.signin_succeeded(
        {"claimed": [{"type": "SIGN_IN"}], "errors": ["签到返回 code=50000"]})
    assert not up.signin_succeeded({"claimed": [], "errors": [], "skipped": True})


def test_banned_account_surfaces_error():
    """banned=true 必须当错误暴露，别静默算签到成功。"""
    fake = FakeStudio(banned=True)
    up = make_upstream(fake)

    res = asyncio.run(up.checkin_once(dry=False))

    assert res["banned"] is True
    assert res["errors"], "封禁账号不该被当成签到成功"
    assert not Upstream.signin_succeeded(res), "封禁不占当日名额"


def test_claim_is_empty_operation():
    """钉住实测结论：claim / complete 返回成功但余额不动。"""
    fake = FakeStudio(popups=[
        {"popupId": 2, "instanceKey": "20261001",
         "componentType": "CLIENT_DOWNLOAD_REWARD_DIALOG"},
    ])
    up = make_upstream(fake)

    # 先签掉，让余额稳定
    asyncio.run(up.checkin_once(dry=False))
    before = fake.granted

    res = asyncio.run(up.checkin_once(dry=False))

    assert "client-download-reward/claim" in " ".join(fake.calls), "应打扫弹窗"
    assert fake.granted == before, "claim 不该发分（它是空操作）"
    assert res["gained"] == 0, f"同日重签应 +0，实得 {res['gained']}"
