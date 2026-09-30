#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""add_account_from_url 的边界测试。只测纯逻辑，不碰网络。

    python -m pytest tests/test_add_account_from_url.py -q
"""
from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
_spec = importlib.util.spec_from_file_location("addacct", ROOT / "tools/add_account_from_url.py")
m = importlib.util.module_from_spec(_spec)
sys.modules["addacct"] = m
_spec.loader.exec_module(m)


# ---------------------------------------------------------------------------
# 解析
# ---------------------------------------------------------------------------

GOOD = "https://www.xfyun.cn/?ssoSessionId=11111111-2222-3333-4444-555555555555&account_id=73010000001"


def test_parse_typical():
    sid, acct = m.parse(GOOD)
    assert sid == "11111111-2222-3333-4444-555555555555"
    assert acct == "73010000001"


def test_parse_strips_quotes():
    sid, acct = m.parse('"%s"' % GOOD)
    assert acct == "73010000001"


def test_parse_order_independent():
    sid, acct = m.parse(
        "https://x.cn/?account_id=73010000001&ssoSessionId=11111111-2222-3333-4444-555555555555")
    assert acct == "73010000001"


def test_parse_rejects_missing_account():
    for bad in (
        "https://www.xfyun.cn/?ssoSessionId=11111111-2222-3333-4444-555555555555",
        "11111111-2222-3333-4444-555555555555",
        "hello world",
        "",
    ):
        try:
            m.parse(bad)
        except SystemExit:
            continue
        raise AssertionError("本该拒绝: %r" % bad)


# ---------------------------------------------------------------------------
# 命名与去重
# ---------------------------------------------------------------------------

def _acct_file(d: Path, stem: str, account_id: str) -> None:
    (d / f"{stem}.json").write_text(
        json.dumps({"accountId": account_id}), encoding="utf-8")


def test_name_uses_first_four_not_last_four(tmp_path):
    # 命名必须取 accountId 前 4 位：取后 4 位会让同一账号生成两份凭据
    name, note = m.resolve_name(tmp_path, "73010000001")
    assert name == "acct-7301", name
    assert note == ""


def test_name_reuses_alias_file_for_same_account(tmp_path):
    # 目录里已有别名（名字≠默认规则），必须续用原文件，避免同一账号两份凭据
    _acct_file(tmp_path, "acct-legacy", "73010000001")
    name, note = m.resolve_name(tmp_path, "73010000001")
    assert name == "acct-legacy"
    assert "续用" in note


def test_name_same_as_default_is_silent(tmp_path):
    # 已有文件叫的就是默认名 → 续用它，但不必提示
    _acct_file(tmp_path, "acct-7301", "73010000001")
    name, note = m.resolve_name(tmp_path, "73010000001")
    assert name == "acct-7301"
    assert note == ""


def test_name_no_duplicate_for_other_account(tmp_path):
    _acct_file(tmp_path, "acct-7301", "73010000001")
    name, note = m.resolve_name(tmp_path, "84020000002")
    assert name == "acct-8402"
    assert note == ""


def test_name_survives_corrupt_files(tmp_path):
    (tmp_path / "broken.json").write_text("{not json", encoding="utf-8")
    _acct_file(tmp_path, "acct-7301", "73010000001")
    name, _ = m.resolve_name(tmp_path, "73010000001")
    assert name == "acct-7301"


def test_name_on_missing_dir(tmp_path):
    name, note = m.resolve_name(tmp_path / "nope", "73010000001")
    assert name == "acct-7301" and note == ""
