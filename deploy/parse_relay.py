#!/usr/bin/env python3
# 逐个模型打真实中继调用并判定结果。由 verify_relay.sh 调用。
import json, sys

for path in sys.argv[1:]:
    name = path.rsplit("/", 1)[-1].replace(".json", "")
    try:
        raw = open(path, encoding="utf-8").read()
        d = json.loads(raw)
    except Exception as e:
        print("  %-26s 解析失败 %s" % (name, str(e)[:60]))
        continue
    if "choices" in d:
        msg = (d.get("choices") or [{}])[0].get("message") or {}
        c = (msg.get("content") or "").strip()
        r = (msg.get("reasoning_content") or "").strip()
        tag = "OK" if c else "空回"
        print("  %-26s %-4s content=%r reasoning=%d字" % (name, tag, c[:20], len(r)))
    else:
        err = d.get("error") or d
        print("  %-26s ERR %s" % (name, str(err)[:110]))
