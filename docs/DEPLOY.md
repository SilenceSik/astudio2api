# 部署指南

把 astudio2api 跑成常驻网关，并接进 new-api。

## 拓扑

```
客户端 / 上层网关
      │
      ▼
  new-api (容器)  ──base_url──▶  astudio2api (宿主, 0.0.0.0:8788)
                                        │  Cookie + Bearer（AStudio 账号会话）
                                        ▼
                                  讯飞 AStudio 上游
                        (agent.xfyun.cn / maas-api.cn-huabei-1.xf-yun.com)
```

**为什么网关要绑 `0.0.0.0` 而不是 `127.0.0.1`**：new-api 跑在容器里，访问宿主
要走 docker 网络的网关地址。查法：

```bash
docker network inspect <net> --format '{{(index .IPAM.Config 0).Gateway}}'
```

绑 `127.0.0.1` 容器就够不到。有安全组/防火墙时只放行该网段即可。

## 落点

| 项目 | 建议路径 | 权限 |
|---|---|---|
| 二进制 | `/opt/astudio2api/bin/astudio2api` | 755 |
| 账号目录 | `/opt/astudio2api/accounts/` | 700，文件 600 |
| 台账 | `/opt/astudio2api/state.json` | 600 |
| 访问密钥 | `/opt/astudio2api/.env`（`ASTUDIO_API_KEY`） | 600 |
| 服务 | `/etc/systemd/system/astudio2api.service` | — |

> ⚠️ 二进制放在 `bin/` 子目录时，**默认账号目录会跟着变成 `bin/accounts`**
> （运行根目录取自可执行文件位置）。诊断特征是「账号文件明明在，`doctor` 却说没有账号」。
> 显式给 `ASTUDIO_ACCOUNTS_DIR` / `ASTUDIO_STATE_PATH` 最省心。

## 步骤

### 1) 交叉编译（在本机）

```bash
cd go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w" -o ../astudio2api-linux-amd64 .
```

### 2) 上传

```bash
ssh root@<你的服务器> 'mkdir -p /opt/astudio2api/{bin,accounts}'
scp astudio2api-linux-amd64 root@<你的服务器>:/opt/astudio2api/bin/astudio2api
ssh root@<你的服务器> 'chmod 755 /opt/astudio2api/bin/astudio2api && \
  chmod 700 /opt/astudio2api/accounts'
```

### 3) 访问密钥

```bash
ssh root@<你的服务器> 'umask 077 && \
  printf "ASTUDIO_API_KEY=%s\n" "$(openssl rand -hex 24)" > /opt/astudio2api/.env'
```

### 4) systemd

```ini
# /etc/systemd/system/astudio2api.service
[Unit]
Description=astudio2api gateway
After=network-online.target

[Service]
EnvironmentFile=/opt/astudio2api/.env
Environment=ASTUDIO_ACCOUNTS_DIR=/opt/astudio2api/accounts
Environment=ASTUDIO_STATE_PATH=/opt/astudio2api/state.json
ExecStart=/opt/astudio2api/bin/astudio2api serve --host 0.0.0.0 --port 8788
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload && systemctl enable --now astudio2api
```

### 5) 加账号

在服务器上直接账号密码登录（不需要浏览器）：

```bash
/opt/astudio2api/bin/astudio2api accounts login --account <手机号>
```

或在本地加好后上传（账号 JSON 是自包含的）：

```bash
scp accounts/<acct>.json root@<你的服务器>:/opt/astudio2api/accounts/
ssh root@<你的服务器> 'chmod 600 /opt/astudio2api/accounts/*.json'
```

新增账号后热加载，不必重启：

```bash
set -a; . /opt/astudio2api/.env; set +a
curl -H "Authorization: Bearer $ASTUDIO_API_KEY" \
  -X POST http://127.0.0.1:8788/admin/reload
```

### 6) 自检

```bash
/opt/astudio2api/bin/astudio2api doctor
```

五项：账号 / 模型目录 / 积分 / 签到 dry-run / **真实推理**。

### 7) 接进 new-api

```bash
export NEWAPI_DB=/opt/new-api/data/new-api.db
export ASTUDIO_ENV_FILE=/opt/astudio2api/.env
export ASTUDIO_HOST_URL=http://<docker 网关 IP>:8788
export NEWAPI_GROUPS=<你的分组>          # 默认 openai
bash deploy/register_channel.sh          # 幂等：同名渠道存在则只更新
```

## 四级验证（逐级收紧）

只测网关不够 —— 网关通了不代表 new-api 能调到。

| 层级 | 手段 | 判定 |
|---|---|---|
| ① 网关自身 | `curl /health`、`curl /v1/chat/completions` | 200 + 有正文 |
| ② 容器视角 | 同网络容器 `curl http://<网关IP>:8788/health` | 200（容器里 `127.0.0.1` **不通**） |
| ③ new-api 渠道 | `GET /api/channel/test/<id>` | `success=true` |
| ④ **真实中继** | `bash deploy/verify_relay.sh` | 逐个模型都有正文 |

> ⚠️ **③ 通过 ≠ ④ 通过。** 渠道测试按 `test_model` 直连，**不走分组路由**，
> 所以测不出「分组 × 模型」映射缺失。改过模型清单后必须跑 ④。

## ⚠️ 改渠道模型时必须同步 `abilities` 表

new-api 靠 `abilities` 表（`分组 × 模型 → 渠道`）路由，它是从 `channels` 派生的，
**直接写 SQL 改 `channels` 不会同步它**。漏了会报：

```
model_not_found: 分组 X 下模型 Y 无可用渠道（distributor）
```

`deploy/register_channel.sh` 会同时重建两张表。手工改的顺序：改 `channels` →
`DELETE FROM abilities WHERE channel_id=<id>` → 按「模型 × 分组」重插 →
`docker restart new-api`。

改前备份 DB；读的时候用只读模式，避免与 new-api 抢锁：

```bash
cp /opt/new-api/data/new-api.db /tmp/new-api.db.bak-$(date +%Y%m%d-%H%M%S)
sqlite3 "file:/opt/new-api/data/new-api.db?mode=ro" 'SELECT ...'
```

## ⚠️ new-api 里的模型名是全局的

多个渠道可声明同名模型，`priority` 高的先接。把上游代号改成干净名（`glm-5.2`）
**可能撞上别的渠道已有的同名模型** → 请求被路由过去，对方返回 404，
看起来却像自己坏了。排查：

```sql
SELECT id,name,priority,models FROM channels WHERE status=1 AND models LIKE '%glm-5.2%';
```

对策是对外名加前缀（`astudio-glm-5.2`）保证全局唯一，再用 `model_mapping`
映回上游代号。

## 运营关注

- **积分会过期**：`astudio2api credits` 看各账号 `activityNextExpireTime`，
  过期即作废。调度器已按「快过期优先」排序，尽量先烧掉要过期的额度。
- **凭据失效**：日志出现登录失效时该账号会被标记下线，需重新
  `accounts login` 或重新导入会话；池子会继续用其它账号服务。
- **每日收益靠自动签到**：网关内置每小时兜底循环，按 `(账号, 日期)` 幂等，
  重复调用不会重复领。真正的发分接口是 `POST tenant-app/v2/init-app`
  （见 README「自动签到」一节，那里记了一个容易踩的误判）。
- **健康度排查**：`GET /health` 看槽位状态；`POST /admin/sync` 会逐账号报
  真实上游错误，是判断「账号是不是真活着」最快的一条路。
