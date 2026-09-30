"""账号池调度算法测试（纯桩件，不打网络）。

验证多账号选择规则：
  零计费优先 → 快过期优先 → 同级轮询 → 黏绑 → 零余额退场 → 不支持排除 → 冷却排除
运行：python tests/test_pool.py
"""

from __future__ import annotations

import asyncio
import sys
import tempfile
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from astudio.accounts import Account  # noqa: E402
from astudio.ledger import Ledger  # noqa: E402
from astudio.pool import Pool, _Slot  # noqa: E402
from astudio.upstream import Upstream  # noqa: E402

FAILED: list[str] = []


def check(name: str, cond: bool, detail: str = "") -> None:
    print(f"  {'✓' if cond else '✗'} {name}" + (f"  —— {detail}" if detail and not cond else ""))
    if not cond:
        FAILED.append(name)


def model(mid: str, mult) -> dict:
    return {"id": mid, "name": mid, "multiplier": mult,
            "base_url": "https://maas-api.cn-huabei-1.xf-yun.com/v1"}


def make_slot(name: str, models: list[dict]) -> _Slot:
    acc = Account(name=name, path=Path(f"/dev/null/{name}.json"),
                  raw={"accountId": f"id-{name}", "uid": f"uid-{name}",
                       "mobile": f"1380000{abs(hash(name)) % 10000:04d}",
                       "modelBearerToken": f"bearer-{name}"})
    up = Upstream(acc, None)  # type: ignore[arg-type] —— 测试不触发网络
    up.models = models
    up.models_synced_at = time.time()
    return _Slot(acc, up)


def build_pool(tmp: Path, spec: dict[str, dict]) -> Pool:
    """spec: {name: {"models": [...], "credits": float|None, "expiry": float|None}}"""
    led = Ledger(tmp / "state.json")
    p = Pool(led)
    for name, cfg in spec.items():
        s = make_slot(name, cfg["models"])
        p._slots.append(s)  # noqa: SLF001
        led.bind_identity(s.identity, name)
        if cfg.get("credits") is not None or cfg.get("expiry") is not None:
            exp = cfg.get("expiry")
            led.update_credits(s.identity, {
                "total": cfg.get("credits"),
                "segments": [{"source": "activity", "total": cfg.get("credits"),
                              "remaining": cfg.get("credits"), "expires_at": exp}],
            })
    return p


def dist(p: Pool, model_id: str, n: int, skey: str | None = None) -> dict[str, int]:
    out: dict[str, int] = {}
    for _ in range(n):
        s = p.pick(skey, model_id)
        k = s.name if s else "<None>"
        out[k] = out.get(k, 0) + 1
    return out


def main() -> int:
    tmp = Path(tempfile.mkdtemp(prefix="astudio-pool-test-"))
    _case = [0]

    def build_iso(spec: dict[str, dict]) -> Pool:  # noqa: F811
        """每个用例独立目录，避免 state.json 串状态。"""
        _case[0] += 1
        d = tmp / f"case{_case[0]:02d}"
        d.mkdir(parents=True, exist_ok=True)
        return build_pool(d, spec)

    build = build_iso  # noqa: F811 —— 用例内统称 build

    print("\n① 零计费优先：A 免费 / B 付费 → 全走 A")
    p = build({
        "A": {"models": [model("m", "x0.00")], "credits": 100, "expiry": 2000},
        "B": {"models": [model("m", "x2.0")], "credits": 100, "expiry": 1000},
    })
    d = dist(p, "m", 12)
    check("全走 A", d.get("A") == 12 and "B" not in d, str(d))

    print("\n② 快过期优先：都付费，A 到期早 → 全走 A")
    p = build({
        "A": {"models": [model("m", "x1.0")], "credits": 100, "expiry": 1000},
        "B": {"models": [model("m", "x1.0")], "credits": 100, "expiry": 9000},
    })
    d = dist(p, "m", 12)
    check("全走 A", d.get("A") == 12 and "B" not in d, str(d))

    print("\n③ 同级轮询：同档（同计费同到期）→ 两号均分")
    p = build({
        "A": {"models": [model("m", "x1.0")], "credits": 100, "expiry": 5000},
        "B": {"models": [model("m", "x1.0")], "credits": 100, "expiry": 5000},
    })
    d = dist(p, "m", 40)
    check("两号都参与", set(d) == {"A", "B"}, str(d))
    check("大致均分(各 30–70%)", all(10 <= v <= 30 for v in d.values()), str(d))

    print("\n④ 黏绑：同一会话键固定同号")
    p = build({
        "A": {"models": [model("m", "x1.0")], "credits": 100, "expiry": 5000},
        "B": {"models": [model("m", "x1.0")], "credits": 100, "expiry": 5000},
    })
    first = p.pick("sess-1", "m")
    same = all(p.pick("sess-1", "m").identity == first.identity for _ in range(10))  # type: ignore[union-attr]
    check("会话键内不漂移", same)
    other = [p.pick(f"sess-{i}", "m") for i in range(10)]
    check("不同会话键可落到不同号",
          len({s.identity for s in other if s}) == 2, str({s.name for s in other if s}))

    print("\n⑤ 零余额退场：余额 0 的号不接付费模型")
    p = build({
        "A": {"models": [model("m", "x2.0")], "credits": 0, "expiry": 1000},
        "B": {"models": [model("m", "x2.0")], "credits": 50, "expiry": 9000},
    })
    d = dist(p, "m", 10)
    check("零余额号被排除", d.get("B") == 10 and "A" not in d, str(d))

    print("\n⑥ 零余额但模型免费 → 仍可用")
    p = build({
        "A": {"models": [model("m", "x0.00")], "credits": 0, "expiry": 1000},
        "B": {"models": [model("m", "x2.0")], "credits": 50, "expiry": 9000},
    })
    d = dist(p, "m", 10)
    check("免费模型走零余额号", d.get("A") == 10 and "B" not in d, str(d))

    print("\n⑦ 不支持该模型的账号被排除")
    p = build({
        "A": {"models": [model("other", "x1.0")], "credits": 100, "expiry": 1000},
        "B": {"models": [model("m", "x1.0")], "credits": 100, "expiry": 9000},
    })
    d = dist(p, "m", 8)
    check("只走支持的 B", d.get("B") == 8 and "A" not in d, str(d))

    print("\n⑧ 冷却排除：A 失效后只剩 B")
    p = build({
        "A": {"models": [model("m", "x1.0")], "credits": 100, "expiry": 1000},
        "B": {"models": [model("m", "x1.0")], "credits": 100, "expiry": 9000},
    })
    slot_a = p.find("A")
    p.note_status(slot_a, 429, "m")  # type: ignore[arg-type]
    d = dist(p, "m", 8)
    check("模型级冷却把 A 排掉", d.get("B") == 8 and "A" not in d, str(d))
    p.note_status(slot_a, 401)  # type: ignore[arg-type]
    check("账号级冷却生效", not slot_a.healthy())  # type: ignore[union-attr]
    p.note_ok(slot_a)  # type: ignore[arg-type]
    check("恢复后重新入选", p.pick(None, "m") is not None)

    print("\n⑧b 403 是模型级权限，不是凭据失效（新账号未订购该模型）")
    p = build({
        "A": {"models": [model("m1", "x1.0"), model("m2", "x1.0")], "credits": 100, "expiry": 1000},
    })
    slot_a = p.find("A")
    p.note_status(slot_a, 403, "m1")  # type: ignore[arg-type]
    check("403 只冷却该模型", p.pick(None, "m1") is None)
    check("同账号其它模型不受牵连", p.pick(None, "m2") is not None)
    check("账号仍健康（凭据没坏）", slot_a.healthy())  # type: ignore[union-attr]

    print("\n⑨ 全部不可用 → 返回 None（快速失败，不打上游）")
    p = build({
        "A": {"models": [model("m", "x1.0")], "credits": 0, "expiry": 1000},
    })
    check("无候选返回 None", p.pick(None, "m") is None)

    print("\n⑩ 无积分数据时不误杀（新账号未同步前仍可用）")
    p = build({"A": {"models": [model("m", "x1.0")]}})
    check("未同步积分 → 可用", p.pick(None, "m") is not None)

    print("\n" + "=" * 60)
    if FAILED:
        print(f"失败 {len(FAILED)} 项：")
        for f in FAILED:
            print(f"  ✗ {f}")
        return 1
    print("全部通过 ✓")
    return 0


if __name__ == "__main__":
    sys.exit(main())
