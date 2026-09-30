#!/usr/bin/env python3
"""astudio2api 入口。

    python astudio2api.py serve            # 起网关 (默认 127.0.0.1:8788)
    python astudio2api.py accounts import  # 导入客户端当前登录的账号
    python astudio2api.py accounts list    # 看池里有哪些账号
    python astudio2api.py doctor           # 自检：账号/目录/积分/签到/推理
    python astudio2api.py checkin          # 手动签到
    python astudio2api.py credits          # 看积分与到期
    python astudio2api.py models           # 看模型

环境变量:
    ASTUDIO_API_KEY        网关访问密钥（设了就要带 Authorization: Bearer）
    ASTUDIO_PORT           监听端口（默认 8788）
    ASTUDIO_ACCOUNTS_DIR   账号目录（默认 <repo>/accounts）
    ASTUDIO_STATE_PATH     台账文件（默认 <repo>/state.json）
    ASTUDIO_SESSION_PATH   客户端活会话文件路径（默认自动探测）
"""

from __future__ import annotations

import sys

from astudio.cli import main

if __name__ == "__main__":
    sys.exit(main())
