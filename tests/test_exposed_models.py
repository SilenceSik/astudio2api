"""对外模型清单口径：必须恰好是标准集，受限模型一个都不能露。

为什么要锁死这个：受限模型（GLM-5.1 / Kimi-K2.6 / Qwen3.6-35B-A3B / MiniMax-M2.5 /
Spark-X2-Agent / Spark-X2-Flash）只有老号有订购权限。一旦对外列出，客户端选到它
就会落到新号上吃 403，还白烧掉该模型 1800s 的冷却额度。
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from astudio.consts import BUILTIN_MODELS, STANDARD_MODEL_IDS  # noqa: E402
from astudio.server import _union_models  # noqa: E402

# 受限模型：只有老号能调，不得出现在对外清单里
RESTRICTED = {
    "xopglm51",
    "xminimaxm25",
    "xopkimik26",
    "xopqwen36v35b",
    "xsparkx2agent",
    "spark-x",
}


class _Up:
    def __init__(self, models):
        self.models = models


class _Slot:
    def __init__(self, models):
        self.up = _Up(models)


class _Pool:
    """老号订阅全量、新号只有三个 —— 模拟真实池子的权限差异。"""

    def __init__(self):
        old = [dict(m) for m in BUILTIN_MODELS] + [
            {"id": "xopglm51", "name": "GLM-5.1", "multiplier": "x2.0"},
            {"id": "xopkimik26", "name": "Kimi-K2.6", "multiplier": "x2.0"},
            {"id": "xminimaxm25", "name": "MiniMax-M2.5", "multiplier": "x1.0"},
        ]
        young = [dict(m) for m in BUILTIN_MODELS]
        self._slots = [_Slot(old), _Slot(young)]

    def slots(self):
        return self._slots


def test_union_models_equals_standard_set():
    got = {m["id"] for m in _union_models(_Pool())}
    assert got == STANDARD_MODEL_IDS, f"对外清单应等于标准集，实得 {sorted(got)}"


def test_restricted_models_never_exposed():
    got = {m["id"] for m in _union_models(_Pool())}
    leaked = got & RESTRICTED
    assert not leaked, f"受限模型不得对外暴露：{sorted(leaked)}"


def test_exposed_ids_are_unique():
    models = _union_models(_Pool())
    ids = [m["id"] for m in models]
    assert len(ids) == len(set(ids)), f"id 重复：{ids}"
    assert len(models) == len(BUILTIN_MODELS)


def test_upstream_multiplier_enriches_standard_entry():
    """上游同步回来的真实倍率要覆盖兜底值，而不是把条目替换掉。"""

    class _P(_Pool):
        def __init__(self):
            self._slots = [_Slot([
                {"id": "spark-x2.5", "name": "Spark-X2.5", "multiplier": "x0.5"},
            ])]

    m = next(x for x in _union_models(_P()) if x["id"] == "spark-x2.5")
    assert m["multiplier"] == "x0.5"
    assert len(_union_models(_P())) == len(BUILTIN_MODELS)
