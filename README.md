# moment_xray_agent

Moment 的新 Agent 负责：

```text
拉取 xray runtime config
拉取用户
热更新用户
上报流量
上报在线用户
上报服务器状态
写入 agent/xray access/error 文件日志
断线后使用 last_good 配置继续运行
内嵌 xray-core 作为 Go 库运行，不依赖 xray 外部二进制或子进程
```

当前阶段已经实现最小 runtime config 与用户快照同步：

```text
推荐启动方式只需要 MOMENT_SERVER_URL + MOMENT_AGENT_ENROLLMENT_KEY
启动后自动向 server 注册运行实例，上报公网 IP、主机名、PID、agent/xray 版本
管理员在 admin 服务页确认运行实例后，agent 使用进程 token 拉取运行配置和用户
启动后立即通过 Connect JSON 拉取 AgentService.GetRuntimeConfig
所有 AgentService 请求都会携带 Authorization: Bearer <process token>
按 MOMENT_AGENT_PULL_INTERVAL 周期拉取 runtime config
校验 xray JSON 后原子写入 current.json
同步更新 last_good.json
初始拉取失败且 current.json 缺失时，尝试从 last_good.json 恢复
启动后立即通过 Connect JSON 拉取 AgentService.GetUsers
按 MOMENT_AGENT_PULL_INTERVAL 周期拉取用户列表
用户版本未变化时跳过写入
校验用户订阅 ID、UUID、password 后原子写入 users_snapshot.json
启动时优先加载本地 users_snapshot.json 作为断线回退用户状态
将有效用户注入到 Xray inbounds 后写入 current.json 与 last_good.json
注入 Xray API、stats、policy 与 api 路由，供后续流量和在线用户统计使用
注入 Xray access/error/loglevel 日志配置
下载 runtime config 中声明的 geoip/geosite 等 geodata 文件，并通过 XRAY_LOCATION_ASSET 指向同一目录
配置变更或用户变更后使用 xray-core 库解析并构造 Instance，成功后重启内嵌运行实例
runtime config 拉取、内嵌实例启动成功后上报 pulled/applied config version
配置解析或内嵌实例启动失败时上报 apply_state=failed、health=error 和失败原因
控制面断线时可以从 current.json/last_good.json 启动上次有效配置
按 MOMENT_AGENT_STATUS_INTERVAL 采集 CPU、内存、磁盘并上报 AgentService.ReportStatus
runtime config 或 users 应用成功后 best-effort 立即上报一次 AgentService.ReportStatus
按 MOMENT_AGENT_TRAFFIC_INTERVAL 通过 Xray StatsService 采集用户流量增量并上报 AgentService.ReportTraffic
上报成功后提交 MOMENT_XRAY_STATS_CURSOR_PATH，本地 cursor 用于控制面临时失败时避免丢失流量
```

可用环境变量：

```text
MOMENT_SERVER_URL
MOMENT_AGENT_ENROLLMENT_KEY
MOMENT_AGENT_PROCESS_UID
MOMENT_AGENT_PUBLIC_IP
MOMENT_AGENT_WORK_DIR
MOMENT_XRAY_CONFIG_PATH
MOMENT_XRAY_LAST_GOOD_PATH
MOMENT_XRAY_USERS_SNAPSHOT_PATH
MOMENT_XRAY_ASSET_DIR
MOMENT_XRAY_INJECT_API
MOMENT_XRAY_API_PORT
MOMENT_XRAY_API_ADDRESS
MOMENT_XRAY_VALIDATE_CONFIG
MOMENT_AGENT_TRAFFIC_INTERVAL
MOMENT_XRAY_STATS_CURSOR_PATH
MOMENT_XRAY_ACCESS_LOG_PATH
MOMENT_XRAY_ERROR_LOG_PATH
MOMENT_XRAY_LOG_LEVEL
```

`MOMENT_AGENT_ENROLLMENT_KEY` 是必填项。旧的 `MOMENT_SERVER_ID` / `MOMENT_AGENT_SECRET` 服务级接入方式已经不再作为运行态鉴权入口使用；agent 必须先用接入 Key 注册进程，由 admin 确认到服务后，server 才会返回进程 token 供后续 runtime RPC 使用。

在线用户会从 Xray stats 用户流量增量中维护近期活跃窗口，并随状态上报同步到 server。用户列表变化时会优先使用内嵌 xray-core 的 inbound UserManager 做细粒度热更新，只增删变化的用户；如果协议、tag、运行状态或 xray-core 动态用户接口不满足条件，会自动回退到“重新渲染有效配置 + 内嵌 xray-core 实例重启”的稳妥路径。

运行：

要求本地 Go 工具链与项目一致：

```bash
go version
# go version go1.26.4 ...
```

```bash
MOMENT_SERVER_URL=https://your-moment-server \
MOMENT_AGENT_ENROLLMENT_KEY=<admin-created-key> \
go run ./cmd/moment-agent
```

启动后到 admin 的“服务 -> Agent 接入”中核对公网 IP、主机名和 PID，并确认到具体服务。随后在服务的 Xray 绑定中选择这个运行实例，保存并写入期望配置；只有 agent 上报 `applied_config_version` 与期望版本一致且 `health=healthy`，才表示机器上的 xray 已真正生效。
