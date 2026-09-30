"""常量与日志（协议来自 AStudio 3.4.4 客户端静态分析）。"""

from __future__ import annotations

import os
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

# -- 上游 ---------------------------------------------------------------------

STUDIO_BASE = "https://agent.xfyun.cn/xingchen-studio/"
DEFAULT_MAAS_BASE = "https://maas-api.cn-huabei-1.xf-yun.com/v1"

# 客户端身份头 —— 缺了会被上游拒绝 (code 11001001)
CLIENT_TYPE_WIN = "21"
CLIENT_TYPE_MAC = "22"

STUDIO_VERSION = "3.4.4"

# 服务端下发的模型清单接口（cookie 认证，用它换 api_key）
MODELS_CONFIG_PATH = "bot/models/configs"

# -- 本地路径 -----------------------------------------------------------------

REPO_DIR = Path(__file__).resolve().parent.parent
ACCOUNTS_DIR = Path(os.environ.get("ASTUDIO_ACCOUNTS_DIR") or (REPO_DIR / "accounts"))
STATE_PATH = Path(os.environ.get("ASTUDIO_STATE_PATH") or (REPO_DIR / "state.json"))

# 活会话文件候选位置（AStudio 客户端写入）
SESSION_CANDIDATES = [
    Path(os.environ.get("ASTUDIO_SESSION_PATH") or r"D:\AStudio Data\userdata\astron-session.json"),
    Path.home() / "AppData/Local/AStudio/userdata/astron-session.json",
    Path.home() / "AppData/Roaming/AStudio/userdata/astron-session.json",
    Path.home() / "Library/Application Support/AStudio/userdata/astron-session.json",
]

# -- 调度参数 -----------------------------------------------------------------

# 黏绑：同一会话键在此时长内复用同一账号
STICKY_TTL = 30 * 60
STICKY_MAX = 4096

# 失败冷却
COOLDOWN_AUTH = 600.0        # 401：凭据失效，冷却 10 分钟
COOLDOWN_RATE = 60.0         # 429：限流
COOLDOWN_ERROR = 30.0        # 5xx / 网络错误
# 403 = 「该账号对这个模型没有订购/额度」，是账号×模型的权限属性，
# 不是凭据失效。只冷却这一个模型，否则会拖垮账号上其它能用的模型。
COOLDOWN_ENTITLEMENT = 1800.0

# 模型目录刷新间隔
MODELS_TTL = 3600.0

# 签到：启动后延迟首签；之后每小时兜底一次（按日幂等，重复调用不重复领奖）
CHECKIN_STARTUP_DELAY = 30.0
CHECKIN_INTERVAL = 3600.0

# 标准模型集：对外暴露的唯一清单（同时兼作服务端拉不到模型表时的兜底）。
#
# 只放三个账号都能用的通用模型。受限模型（GLM-5.1 / Kimi-K2.6 / Qwen3.6-35B-A3B /
# MiniMax-M2.5 / Spark-X2-Agent / Spark-X2-Flash）只有老号有订购权限，对外列出
# 会让请求落到新号上吃 403 并白烧该模型冷却。它们仍能用原始 slug 直接调用。
BUILTIN_MODELS: list[dict[str, Any]] = [
    {"id": "spark-x2.5", "name": "Spark-X2.5", "multiplier": None, "base_url": DEFAULT_MAAS_BASE},
    {"id": "xopglm52", "name": "GLM-5.2", "multiplier": "x2.0", "base_url": DEFAULT_MAAS_BASE},
    # 客户端真实目录里的 slug 带日期后缀（xopdeepseekv4pro0813）；
    # 裸 slug xopdeepseekv4pro 也仍通，但不要同时放进表里 —— 两者映射到同一
    # 友好名，会在 /v1/models 产出两个同 id 条目。
    {"id": "xopdeepseekv4pro0813", "name": "DeepSeek-V4-Pro", "multiplier": "x2.0",
     "base_url": DEFAULT_MAAS_BASE},
    {"id": "xopdsv4flash0731in", "name": "DeepSeek-V4-Flash", "multiplier": "x1.0",
     "base_url": DEFAULT_MAAS_BASE},
]

# 标准集 id 快照，供对外清单过滤用。
STANDARD_MODEL_IDS = {m["id"] for m in BUILTIN_MODELS}

# 签到「一天」的判定时区 = 上游时区（北京时间）。
#
# 不能用宿主本地时区：上游按北京时间算日界，若宿主是 UTC（容器镜像默认就是
# UTC），北京时间 00:00–08:00 会被算成前一天，与上游的按天幂等窗口错位 ——
# 该区间内签到会被判成「昨日已领」而跳过。国内机器是 CST 时看不出问题，
# 跑在 UTC 容器里就出错，所以这里显式钉死（与 Go 版 CheckinTZ 对齐）。
CHECKIN_TZ = timezone(timedelta(hours=8))


def checkin_day(now: datetime | None = None) -> str:
    """把时刻折算成上游口径的日期串（YYYY-MM-DD）。"""
    t = now or datetime.now(CHECKIN_TZ)
    return t.astimezone(CHECKIN_TZ).strftime("%Y-%m-%d")


def log(msg: str, *args: Any) -> None:
    ts = datetime.now().strftime("%H:%M:%S")
    print(f"[{ts}] {msg % args if args else msg}", flush=True)


def multiplier_is_free(mult: Any) -> bool:
    """倍率是否为零计费（x0 / x0.00 / 0 之类）。未知倍率不算免费。"""
    if mult is None:
        return False
    s = str(mult).strip().lstrip("xX")
    try:
        return float(s) == 0.0
    except ValueError:
        return False
