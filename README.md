# Moment Xray Agent

Moment Xray Agent 运行在节点机器上，负责接入 Moment Server、拉取运行配置和用户快照、运行内嵌 xray-core、上报状态、流量和在线用户。这个目录可以作为独立 GitHub 仓库发布。

## Runtime Model

Agent 启动只需要 server 地址和 enrollment key：

```bash
MOMENT_SERVER_URL=https://api.example.com \
MOMENT_AGENT_ENROLLMENT_KEY=<admin-created-key> \
go run ./cmd/moment-agent
```

启动后 Agent 会自动注册运行实例并上报公网 IP、主机名、PID、Agent 版本和 xray-core 版本。管理员在 admin 的“服务”页面确认该运行实例后，server 才会下发进程 token；后续所有 Agent RPC 都使用该 token 鉴权。

## Responsibilities

```text
拉取 xray runtime config
拉取用户快照
热更新用户
运行内嵌 xray-core
下载 geodata 资源
写入 current/last_good 配置
上报 pulled/applied config version
上报流量、在线用户和运行状态
写入 agent/xray access/error 文件日志
控制面断线后继续使用 last_good 配置运行
```

Agent 不依赖外部 xray 二进制或子进程；xray-core 作为 Go 库内嵌运行。

## Requirements

```text
Go 1.26.4
Linux host network access for inbound ports
```

## Environment Variables

```text
MOMENT_SERVER_URL
MOMENT_AGENT_ENROLLMENT_KEY
MOMENT_AGENT_PROCESS_UID
MOMENT_AGENT_PUBLIC_IP
MOMENT_AGENT_WORK_DIR
MOMENT_AGENT_PULL_INTERVAL
MOMENT_AGENT_STATUS_INTERVAL
MOMENT_AGENT_TRAFFIC_INTERVAL
MOMENT_XRAY_CONFIG_PATH
MOMENT_XRAY_LAST_GOOD_PATH
MOMENT_XRAY_USERS_SNAPSHOT_PATH
MOMENT_XRAY_ASSET_DIR
MOMENT_XRAY_INJECT_API
MOMENT_XRAY_API_ADDRESS
MOMENT_XRAY_API_PORT
MOMENT_XRAY_VALIDATE_CONFIG
MOMENT_XRAY_STATS_CURSOR_PATH
MOMENT_XRAY_ACCESS_LOG_PATH
MOMENT_XRAY_ERROR_LOG_PATH
MOMENT_XRAY_LOG_LEVEL
```

`MOMENT_SERVER_URL` 和 `MOMENT_AGENT_ENROLLMENT_KEY` 是必填项。其他路径类配置默认写入 Agent 工作目录。

## Runtime Files

```text
current.json          当前运行配置
last_good.json        最近一次成功运行配置
users_snapshot.json   最近一次用户快照
stats_cursor.json     流量上报 cursor
agent.log             Agent 运行日志
xray-access.log       xray access 日志
xray-error.log        xray error 日志
```

配置或用户变化后，Agent 会先校验并构造新的 xray-core instance。成功后上报 `apply_state=applied`、`health=healthy` 和 `applied_config_version`；失败时保留旧配置继续运行，并上报 `apply_state=failed`、`health=error` 和 `last_error`。

## User Hot Update

用户列表变化时，Agent 会优先使用 xray-core inbound UserManager 做细粒度热更新，只增删变化的用户。如果协议、tag、运行状态或动态用户接口不满足条件，会回退到“重新渲染有效配置 + 重启内嵌 xray-core instance”的稳妥路径。

## Validate

```bash
go test ./... -count=1
```

## Docker

```bash
docker build -t moment-xray-agent:local .
```

GitHub workflow 会构建并推送：

```text
ghcr.io/<owner>/moment-xray-agent:<tag>
```
