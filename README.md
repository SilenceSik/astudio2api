# astudio2api

把**讯飞 AStudio（星辰 Studio）客户端的账号池**变成 OpenAI 兼容 API —— 多账号负载、自动签到领积分。

协议逆向自 AStudio 3.4.4 客户端安装包（Electron + NSIS），**无需抓包、无需修改客户端**。

> **设计参考**：[maiphucgiang/codebuddy2api](https://github.com/maiphucgiang/codebuddy2api)（MIT）——
> 多账号池、积分台账与自动签到的架构思路来自该项目。本项目面向讯飞 AStudio，
> 协议、模型与账号体系均为独立实现，与该项目无代码依赖关系。

## 引用的项目

| 项目 | 许可 | 参考了什么 |
|---|---|---|
| [maiphucgiang/codebuddy2api](https://github.com/maiphucgiang/codebuddy2api) | MIT | 多账号池调度、积分台账、自动签到的整体设计 |

## 特性

- **OpenAI 兼容**：`/v1/chat/completions`（流式 + 非流式）、`/v1/responses`、`/v1/models`
- **上游原生 OpenAI 协议**，不做格式转换，透传即可
- **两种实现，台账互通**（同一个 `state.json` 可互相接管）
  - `go/` —— **生产版**，单二进制、纯标准库、零依赖
  - `astudio/` —— Python 参考版，留作协议对照与快速探针
- **多账号池**：零计费优先 → 快过期优先 → 同级轮询，会话级黏绑
- **自动签到**：按 `(账号, 日期)` 幂等，领取前后用累计发放量自测战果
- **模型名归一**：`GLM-5.2`、`glm 5.2`、`xopglm52` 都能命中同一个模型
- **失效隔离**：401 冷却账号、403 只冷却该模型（不牵连账号上其它模型）

## 快速开始

### Go 生产版（推荐）

```bash
cd go
go build -o astudio2api .        # 纯标准库，无第三方依赖

./astudio2api accounts login --account <手机号>   # 加账号（密码免回显，不落盘）
./astudio2api doctor                             # 五项自检：账号/目录/积分/签到/推理
./astudio2api serve                              # 起网关 → 127.0.0.1:8788
```

### Python 参考版

```bash
pip install -r requirements.txt

python astudio2api.py accounts import    # 导入客户端当前登录的账号
python astudio2api.py doctor
python astudio2api.py serve
```

然后任何 OpenAI 客户端改 `base_url` 即可：

```bash
curl http://127.0.0.1:8788/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}]}'
```

## 加账号

三种方式，任选：

| 方式 | 命令 | 说明 |
|---|---|---|
| **账号密码**（首选） | `astudio2api accounts login --account <手机号>` | 服务端直登，不需要浏览器、不需要装客户端 |
| 地址栏 URL | `python tools/add_account_from_url.py '<URL>'` | 网页登录后把整串 URL 粘进来，自动抠出凭证并验活 |
| 客户端会话 | `astudio2api accounts import` | 读 AStudio 客户端当前登录态（需本机装了客户端） |

密码登录走讯飞统一认证的 `SSOService/login/check-account`（**GET**，写成 POST 会吃 405）。
账号命中极验滑块时服务端会先返回 `if-captcha=true`，此时只能改用后两种方式。

新增账号按 `accountId` 去重：同一账号重复导入会更新原文件，不会在池里出现两份凭据
（两份凭据会让该账号的调度权重翻倍）。

## 模型

**对外暴露的只有各账号都通用的模型**，避免请求落到没有订购权限的账号上：

| 对外名 | 上游代号 | 倍率 |
|---|---|---|
| `spark-x2.5` | `spark-x2.5` | — |
| `glm-5.2` | `xopglm52` | ×2.0 |
| `deepseek-v4-pro` | `xopdeepseekv4pro0813` | ×2.0 |
| `deepseek-v4-flash` | `xopdsv4flash0731in` | ×1.0 |

其余模型（`GLM-5.1`、`Kimi-K2.6`、`Qwen3.6-35B-A3B`、`MiniMax-M2.5`、
`Spark-X2-Agent`、`Spark-X2-Flash`、`Auto`）**不对外列出，但仍可直接按名调用**。

> 为什么这样切分：这些模型的订购权限**按账号而异**。若一律对外列出，
> 请求被调度到没有权限的账号上就会吃 `403 code 11200`，还白烧掉该模型的冷却额度。
> 所以**暴露要窄，解析要全** —— 两个关注点是分开实现的
> （`unionModels()` 管暴露，`aliasModels()` 管解析）。

### ⚠️ 思考型模型的空回复陷阱

默认开启思考的模型（`spark-x2.5`、`Spark-X2-Agent`、`MiniMax-M2.5` 等）在
`max_tokens` 偏小时，token 会被思考过程吃光，`content` 返回**空串** ——
看起来像模型坏了，其实不是。以 `spark-x2.5` 为例：

| 调用方式 | 结果 |
|---|---|
| 默认 + `max_tokens=64` | `content=''`，reasoning 126 字，用满 64 token |
| `reasoning_effort: "none"` | `content='收到'`，2 token ✅ |
| 默认 + `max_tokens=2048` | `content='收到'` + reasoning 113 字 ✅ |

**排查模型可用性时一律带 `reasoning_effort:"none"`**，否则会把好模型误判成坏的。

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `ASTUDIO_API_KEY` | 空（不校验） | 网关访问密钥；对外监听前**务必设置** |
| `ASTUDIO_PORT` | `8788` | 监听端口 |
| `ASTUDIO_HOST` | `127.0.0.1` | 监听地址 |
| `ASTUDIO_ACCOUNTS_DIR` | `<repo>/accounts` | 账号目录 |
| `ASTUDIO_STATE_PATH` | `<repo>/state.json` | 台账文件 |
| `ASTUDIO_SESSION_PATH` | 自动探测 | 客户端活会话文件 |

> 二进制放在 `bin/` 子目录时，默认账号目录会跟着变成 `bin/accounts`，
> 此时要显式给 `ASTUDIO_ACCOUNTS_DIR`。

## 命令行

| 命令 | 作用 |
|---|---|
| `accounts list` | 看池里有哪些账号 |
| `accounts login --account A [--password P]` | 账号密码登录并入库 |
| `accounts import [--name N]` | 导入客户端当前登录的账号 |
| `accounts remove --name N` | 移除账号 |
| `models` | 各账号可用的模型与倍率 |
| `credits` | 各账号积分、分段与到期时间 |
| `checkin [--dry] [--force]` | 手动签到领积分 |
| `sync` | 刷新全部账号的模型目录与积分 |
| `doctor` | 五项自检（账号 / 目录 / 积分 / 签到 / 真实推理） |
| `serve` | 启动网关 |

## HTTP 接口

**OpenAI 兼容**：`/v1/models`、`/v1/chat/completions`、`/v1/responses`

**管理面**：

| 接口 | 作用 |
|---|---|
| `GET /health` | 存活 + 账号健康（无需鉴权） |
| `GET /admin/accounts` | 账号列表：模型数、积分、签到状态、冷却 |
| `POST /admin/accounts/login` | 账号密码登录入库 |
| `POST /admin/accounts/import` | 导入账号（`?name=` / `?path=`） |
| `DELETE /admin/accounts/{name}` | 移除账号 |
| `GET /admin/credits` | 全池积分汇总（含分段与到期） |
| `GET /admin/points?account=N` | 单账号实时余额 |
| `GET /admin/models` | 按账号看模型供给 |
| `POST /admin/checkin` | 手动签到（`?dry=true` 只看不领，`?force=true` 忽略当日幂等） |
| `POST /admin/sync` | 刷新目录与积分（逐账号报真实上游错误，查账号存活最快的一条路） |
| `POST /admin/reload` | 重扫账号目录 |

## 账号池调度

1. **零计费优先** —— 该账号目录里此模型标免费（`x0.00`）的排最前
2. **快过期优先** —— 同档内积分最早到期的排前，避免过期作废
3. **同级轮询** —— 完全同档的账号之间轮询分摊
4. **会话黏绑** —— 同一会话键 30 分钟内固定同一账号
5. **零余额退场** —— 余额为 0 的账号只接自己目录里标免费的模型

失败处理：**401 冷却账号 10 分钟**；**403 只冷却该模型 30 分钟**；429 冷却该模型
60 秒；5xx 冷却 30 秒。单次请求最多换 3 个账号重试。

> 为什么 403 要按模型级而非账号级冷却：403 是「该账号未订购此模型」，
> 账号上其它模型仍然可用。若当账号级失效把账号整体下线，一个未订购的模型
> 就会拖死该账号上所有可用模型。

## 自动签到

- 网关启动 30 秒后首签，之后**每小时兜底**（按 `(账号, 日期)` 幂等）
- 真正发分的接口是 **`POST tenant-app/v2/init-app`**（客户端启动登录时调的就是它），
  一次发两笔：常规每日登录积分（体验版 100 / 标准版 200 / 高级版 400）+
  活动期加成（例如国庆额外 5000 星火积分）
- 计量用 **`totalAmount`（累计发放，单调递增）**，不是 `totalBalance`（会被调用消耗）
- **「一天」按北京时间（上游时区）算**，不跟随宿主时区 —— 跑在 UTC 容器里也不会
  在北京时间 00:00–08:00 把签到算成前一天
- 手动触发：`POST /admin/checkin?dry=true` / `?force=true`

> **易错点（重要）**：签到**不是**走 `client-popups/pending` → `client-popups/complete`。
> 那两个弹窗接口只打扫 UI 状态、一分不发（连调两次都返回成功而余额不动），
> 客户端自己也从不调用它们。真正发分的是 `tenant-app/v2/init-app`。
> 若按弹窗接口记账，会把当天写成 `ok=true, claimed=0` 从而**当天不再重试**，
> 表现为新账号静默漏签、且台账看起来是成功的。

## 协议要点（排查时看这里）

| 项 | 值 |
|---|---|
| 账号站 | `https://agent.xfyun.cn/xingchen-studio/` |
| 模型网关 | `https://maas-api.cn-huabei-1.xf-yun.com/v1`（配置下发的 `/v2` 也通） |
| **必需请求头** | `clientType: 21`(Win) / `22`(mac)、`studioVersion: 3.4.4` |
| 会话文件 | `<state>/userdata/astron-session.json` |
| **token 刷新** | `GET bot/models/configs`（cookie 认证）→ 各模型的 `api_key` |
| 模型目录缓存 | `<state>/userdata/provider-model-catalog-v1.json` |
| **签到（真正发分）** | `POST tenant-app/v2/init-app` |
| 弹窗（空操作） | `client-popups/pending`、`client-download-reward/claim`、`client-popups/complete` |
| 积分 | `points/balance`、`points/summary`、`points/details?pageNum=&pageSize=` |
| 套餐 | `membership/me` |
| 账号密码登录 | `GET https://sso.xfyun.cn/SSOService/login/check-account?accountName=&accountPwd=&isAct=false` |

**状态目录不在用户 home**：AStudio 装在 `D:\AStudio` 时，状态在 `D:\AStudio Data\`
（与安装目录同级，不是 `%APPDATA%`）。不确定时读进程命令行里的 `--user-data-dir=`。

**两份模型清单内容不同，别拿错**：

| 来源 | 内容 | 用途 |
|---|---|---|
| `bot/models/configs` | 较全（含只有部分账号有权限的） | 取 `api_key` |
| `provider-model-catalog-v1.json` | 客户端选择器实际展示的那几个 | 判断「客户端能用哪些」 |

## 部署

见 [docs/DEPLOY.md](docs/DEPLOY.md)（从零到接进 new-api 的完整步骤）。
**维护 / 改动影响面 / 排障**见 [docs/MAINTENANCE.md](docs/MAINTENANCE.md) ——
改任何东西之前值得先扫一眼它的「改动影响面」表，这个项目的坑基本都在「只改了一处」。

`deploy/` 下的脚本都从环境变量取值，默认值只是按推荐部署布局给的推测，
路径不同时用环境变量覆盖即可：

```bash
export ASTUDIO_SSH_HOST=<你的服务器>
export ASTUDIO_HOST_URL=http://<docker 网关 IP>:8788
export NEWAPI_DB=/opt/new-api/data/new-api.db
bash deploy/register_channel.sh     # 注册/更新 new-api 渠道（幂等）
bash deploy/verify_channel.sh       # new-api 侧权威验证
bash deploy/verify_models.sh        # 逐个模型走容器路径验证
bash deploy/verify_relay.sh         # 逐个模型走 new-api 真实中继验证
```

### ⚠️ 改 new-api 渠道模型时，`abilities` 表必须一起改

new-api 的路由靠 `abilities` 表做「**分组 × 模型 → 渠道**」映射，它是从 `channels`
派生的但**不会自动同步**。只改 `channels.models` 不改 `abilities`，重启后会报
`分组 X 下模型 Y 无可用渠道（distributor）`。

**而 `/api/channel/test/<id>` 依然会通过** —— 它按 `test_model` 直连、不走分组路由。
所以**渠道测试通过 ≠ 客户端能调到**，改完必须跑真实中继。

## 测试

```bash
cd go && go test ./...        # Go：调度 / 签到台账 / 模型名解析 / 对外清单口径
python -m pytest tests/ -q    # Python：调度 / 签到 / 加号去重 / 对外清单口径
python astudio2api.py doctor  # 端到端五链路自检（需真实账号）
```

## 已知边界

- 这是**逆向实现**，不是官方接口，上游改协议即失效；`doctor` 用于快速定位坏在哪一环
- 积分按倍率消耗且有到期时间（`credits` 可查 `activityNextExpireTime`），过期即作废
- `Spark-X2-Flash`（`spark-x`）走讯飞自家端点，响应多包一层 `status/sid`，
  且在部分账号下即使关闭思考仍返回空响应，故未列入对外清单
- 账号命中极验滑块时密码登录不可用，需改用 URL 导入或客户端会话导入

## 免责声明

- 本项目仅供**个人账号自用与协议研究**，系独立实现，与讯飞官方无关。
- 请遵守讯飞的服务条款，不要用于批量注册、转售或任何违反条款的用途。
- 本项目基于特定客户端版本与链路测试验证，上游协议变更即可能失效，
  不同账号的权益与模型可用性亦存在差异；使用产生的账号风险由使用者自行承担。

## License

MIT，见 [LICENSE](LICENSE)。

## Community

感谢 [LINUX DO](https://linux.do) 社区提供开放友好的技术讨论平台。

