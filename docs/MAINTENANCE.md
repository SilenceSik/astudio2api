# 维护手册

回答的是**「要改 X 时，必须同时动哪些地方」**。
「怎么从零部署」见 [`DEPLOY.md`](./DEPLOY.md)，两者互补不重复。

改之前先扫一眼 §2 的影响面表 —— 这个项目绝大多数事故都是「只改了一处，另一处没跟上」。

---

## 1. 文档导航

| 文档 | 讲什么 |
|---|---|
| `README.md` | 这是什么、怎么跑起来、模型清单 |
| `docs/DEPLOY.md` | 从零部署到接进 new-api 的完整步骤 |
| `docs/MAINTENANCE.md`（本文） | 改动的**影响面**、**不变式**、排障对照 |

---

## 2. 改动影响面（最重要的一节）

| 要改什么 | 必须同时动的地方 | 漏了会怎样 |
|---|---|---|
| **对外暴露的模型清单** | ① `go/config.go` `builtinModels()` ② `astudio/consts.py` `BUILTIN_MODELS` ③ 渠道 `channels.models` ④ `abilities` 派生表 ⑤ `channels.test_model` ⑥ README/DEPLOY 的模型表 | ①漏→两侧口径不一；③④漏→`分组 X 下模型 Y 无可用渠道`；⑤漏→渠道测试恒红 |
| **模型别名解析** | `go/config.go` `aliasModels()`（**解析面要保持全量**，比暴露面宽） | 收窄解析面 = 回退，本来能调的变成 404 |
| **调度冷却参数** | `go/config.go` 顶部常量 + `astudio/consts.py` 对应值 | 两侧行为分叉，Python 版变成误导 |
| **签到逻辑** | 两侧实现 + 回归测试；**测试别硬编码日期**，用 `CheckinDay()` / `checkin_day()` | 测试随宿主时区飘，CI（UTC）红而本机绿 |
| **渠道分组 / 优先级** | `channels."group"` / `priority` **和** `abilities."group"` | abilities 不同步 → 新分组下无可用渠道 |
| **网关密钥** | 服务器 `.env`（权限 600）+ new-api 渠道 `key` 字段 | 改了 .env 不改渠道 → 401 |
| **账号增删** | `accounts/*.json` + `state.json`，然后 `/admin/reload` 热加载 | 不 reload 要等下次重启才生效 |
| **换二进制** | 备份 → 传 → **校验 sha256** → 重启 → 四级验证（§4） | 见 §4 |

---

## 3. 不变式（破坏了会静默坏）

### 3.1 模型名的三层身份

| 层 | 例子 | 在哪 |
|---|---|---|
| 上游代号（内部 slug） | `xopglm52` | 上游只认这个 |
| 展示名 | `GLM-5.2` | 源码里的 `Name` 字段 |
| **对外友好名** | `glm-5.2` | `/v1/models` 暴露、客户端实际用的 |

- `/v1/models` 暴露的友好名由 `friendlyModelName(Name, ID)` 生成：**优先取展示名**
  （空则退回上游代号）→ 转小写 → 非字母数字（`.` 除外）的连续串折成一个 `-` → 去首尾 `-`。
  例：`GLM-5.2` → `glm-5.2`、`Spark-X2.5` → `spark-x2.5`
- 名字过于泛的（`auto` / `chat` / `default` / `model` / `gpt`，见 `genericModelNames`）
  **会被加前缀**以避开别的 provider 撞名 —— 改暴露面时别漏这层
- `canonicalModel()` 负责「客户端写什么 → 上游代号」：认友好名、认裸代号，
  大小写与 `.`/`-`/`_`/空格 的差别都会归一（`GLM_5_2`、`glm.5.2`、`glm 5.2` 命中同一个）
- **对外暴露 4 个通用模型**（实测各账号通用、不挑账号）：
  `spark-x2.5` / `glm-5.2` / `deepseek-v4-pro` / `deepseek-v4-flash`
- **其余模型可用性**取决于**该账号的订购权限**，不对外列出。不同账号能调的集合不同
  （受限项可能含 `GLM-5.1` / `Kimi-K2.6` / `Qwen3.6-35B-A3B` / `MiniMax-M2.5` /
  `Spark-X2-Agent` / `Spark-X2-Flash` / `Auto` 等）；**具体哪几个可用按账号而变，
  不要假定**。调用一个该账号未订购的模型 → 403，只冷却该模型 30 分钟（见 §3.3）。
- 它们**仍可被指名调用**（`canonicalModel` 找不到就原样透传上游），所以解析面必须保留。

> **别名表有一个刻意的重复**：`xopdeepseekv4pro0813`（标准集）与裸 slug
> `xopdeepseekv4pro`（仅解析面）映射到**同一个友好名** `deepseek-v4-pro`。
> 前者进 `builtinModels()` 参与暴露，后者只进 `aliasModels()` 参与解析 ——
> 若把裸 slug 也放进暴露集，`/v1/models` 会产出两条同 id 条目。
> 所以两个函数长度为 12 和 4，**别看到数字不等就「修」**。

> **解析与暴露必须分开**：解析要全（否则是回退），暴露要窄（否则请求落到没订购的账号上白吃一次 403 冷却）。

### 3.2 签到

- **「一天」按北京时间算**，不跟宿主时区：`CheckinTZ` / `CHECKIN_TZ`
  （容器镜像默认 UTC，北京时间 00:00–08:00 用本地时区会算成前一天）
- **计量用 `totalAmount`（累计发放，单调递增）**，不是 `totalBalance`（会被调用消耗）
- 幂等键 = `(账号, 日期)`
- 真正发分的接口只有一个：`POST tenant-app/v2/init-app`。
  弹窗类接口（`client-popups/*`、`client-download-reward/claim`）**是空操作**，
  误当签到会把台账写成「今日已领」且永不重试 —— 新号静默漏签
- 网关内置每小时兜底循环（`CheckinInterval = 3600s`）

### 3.3 账号池与冷却

| 触发 | 冷却 | 作用域 |
|---|---|---|
| HTTP 401 | `CooldownAuth` 600s | 整个账号 |
| HTTP 429 | `CooldownRate` 60s | 整个账号 |
| 5xx / 网络 | `CooldownError` 30s | 整个账号 |
| **HTTP 403** | `CooldownEntitlement` 1800s | **只冷却该模型** |

**403 必须只冷却模型，不能冷却账号** —— 403 的含义是「这个账号没订购这个模型」，
不是「账号坏了」。按账号冷却会让一个未订购模型把该账号上其它能用的模型一起拖死。

**`code=80000` 要当登录失效**：上游会话失效时返回 **HTTP 200** + `{"code":80000,...}`。
必须识别成 `ErrAuthExpired` 并标记 `CredDead`，否则死账号会被 `/health` 谎报健康、继续派单。

### 3.4 凭据

账号 JSON 字段（**这些是密钥，任何文档 / 截图 / 日志里都不许出现真实值**）：

```
accountId / uid / token / ssoSessionId / modelBearerToken / banned / loginMethod / loggedInAt
```

- 最少只需 `ssoSessionId` 一个 cookie 就能换取上游 `modelBearerToken`
- 按 `accountId` 去重，同一账号重复导入只更新不新增
- `token` / `ssoSessionId` / `modelBearerToken` 属敏感字段；`accounts/` 与 `state.json`
  **必须留在 `.gitignore` 里**，绝不要 `git add -f`

---

## 4. 验证与回滚

### 四级验证（逐级收紧，别跳级）

| 级别 | 手段 | 通过标准 |
|---|---|---|
| ① 进程 | `systemctl is-active astudio2api` | `active` |
| ② 网关自身 | `curl :8788/health` | `{"status":"ok"}`，`healthy` == `accounts` |
| ③ 网关直调 | `POST /v1/chat/completions`（带 `ASTUDIO_API_KEY`） | 拿到真实正文 |
| ④ **中继** | 走 new-api 真实调用 | 拿到真实正文 |

> ③ 要带 `"reasoning_effort":"none"`，否则思维链模型（`spark-x2.5` 等）可能只出思考、正文为空。

**③ 通过 ≠ ④ 通过**：`/api/channel/test/<id>` 按 `test_model` 直连、**不走分组路由**，
`abilities` 没同步时渠道测试照样绿，客户端却调不到。**必须跑 ④。**

### 回滚

```bash
# 换二进制前先备份
cp $PREFIX/bin/astudio2api $PREFIX/bin/astudio2api.bak-$(date +%Y%m%d-%H%M%S)
# 回滚
cp $PREFIX/bin/astudio2api.bak-<时间戳> $PREFIX/bin/astudio2api
systemctl restart astudio2api
```

---

## 5. 排障对照表

| 现象 | 先查 | 常见原因 |
|---|---|---|
| 渠道测试报 `没有可用账号` | `channels.test_model` | `test_model` 不是网关暴露的名字之一（**别被误导成服务挂了 —— 真实调用往往正常**） |
| 客户端报 `分组 X 下模型 Y 无可用渠道` | `abilities` 表 | 改了 `channels.models` 没重建 abilities |
| 客户端调不到，但渠道测试绿 | 同上 | 同上（两条路） |
| 请求被路由到别的渠道 | 各渠道 `priority` + `abilities.group` | 同名模型被更高 priority 的渠道抢走 |
| `/health` 说健康但调用失败 | 账号是否 `needs_relogin` | 会话失效返回 200 + `code=80000`，漏判 |
| 某个模型报错后整个账号下线 | 403 处理 | 403 被误当 401（应只冷却该模型） |
| 新号积分不涨 | 签到走的是不是 `init-app` | 误用空操作接口，台账写死「已领」 |
| 北京时间凌晨签到说「今日已领」 | 日界时区 | 宿主 UTC，日界没钉北京时区 |
| 思维链模型返回空正文 | 请求参数 | 没给 `reasoning_effort:"none"` 或 token 预算不足 |
| 模型名解析不出 | `aliasModels()` | 解析面被误收窄 |
| 单账号限流 | 账号数 / sticky 亲和 | 账号少且同一会话粘在同一账号 |

---

## 6. 常用命令

```bash
# 本机
cd go && go test -count=1 ./...          # Go（-count=1 必带，否则走缓存得假绿）
TZ=UTC go test -count=1 ./...            # 复现 CI（Ubuntu/UTC）环境
python -m pytest tests/ -q               # Python
python astudio2api.py doctor             # 端到端五链路自检（需真实账号）

# 服务器
systemctl restart astudio2api
journalctl -u astudio2api -n 50 --no-pager -o cat
curl -s :8788/health
```

管理端点（都需 `ASTUDIO_API_KEY`）：
`/admin/accounts`、`/admin/accounts/import`、`/admin/accounts/login`、
`/admin/reload`、`/admin/sync`、`/admin/checkin?dry=true|force=true`、
`/admin/credits`、`/admin/credits/refresh`、`/admin/points`、`/admin/models`

CLI：`serve` / `accounts list|import|login|remove` / `models` / `credits` /
`checkin [--dry] [--force]` / `sync` / `doctor`

环境变量：`ASTUDIO_API_KEY` / `ASTUDIO_ACCOUNTS_DIR` / `ASTUDIO_STATE_PATH` /
`ASTUDIO_PORT` / `ASTUDIO_SESSION_PATH`

---

## 7. 已知边界

- **逆向实现**，不是官方接口；上游改协议即失效。`doctor` 用于快速定位坏在哪一环。
- 积分**按倍率消耗且有到期时间**（`credits` 看 `activityNextExpireTime`），过期作废。
  调度器按「快过期优先」排序，尽量先烧要过期的额度。
- 受限模型依赖账号订购权限，调用未订购的模型会 403 并冷却该模型 30 分钟。
- `spark-x`（Spark-X2-Flash）走上游自家端点，响应多包一层 `status/sid`。
