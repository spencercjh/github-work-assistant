# GitHub Work Assistant for Raft

把你关注的公开 GitHub Issue、PR、评审和 CI 变化送到 Raft，由你指定的执行 Agent 判断哪些变化值得提醒，或继续已获授权的仓库工作。

每人部署一套。程序使用你已有的 `gh` 登录采集事实，以独立 **External Agent** 身份向执行 Agent 发消息；执行 Agent 在同一台机器读取本地证据。程序不调用模型，模型和工作执行由 Raft 管理。

```mermaid
flowchart LR
  GH[GitHub Notifications / Issue / PR] -->|只读 API| C[本机 Go 采集程序]
  C --> DB[(SQLite / MySQL / PostgreSQL：快照、待发事件、处理记录)]
  C -->|External Agent 消息，出站 HTTPS| R[Raft 执行 Agent]
  R -->|本机 CLI| DB
  R --> U[向用户汇报 / 继续已授权工作]
```

## 快速开始

在运行 Raft 执行 Agent 的 Linux 电脑上，使用同一个操作系统账号准备 Go 1.26+ 和 GitHub CLI。默认使用本地 SQLite 文件；选择新建 MySQL 容器时才需要 Docker 和 Compose 插件。用 `gh auth login --hostname github.com` 登录自己的 GitHub 账号。在 Raft 创建一个独立的 External Agent，并取得它的登录 token；另外选择一个已有的执行 Agent。两者不能是同一身份。

```bash
gh repo clone spencercjh/github-work-assistant
cd github-work-assistant
make setup
```

安装向导先选择存储位置：本地 SQLite、新建 MySQL 容器，或已有 MySQL／seekdb／TiDB／PostgreSQL。随后填写执行 Agent 名称、可选的通知收件人，并隐藏输入 External Agent token。向导核对身份、生成私有配置、初始化表结构并执行诊断。已有安装不会被覆盖。各后端的连接方式和验证范围见[存储配置](docs/STORAGE.md)。

把生成的 `state/assistant/EXECUTOR.md` 路径交给执行 Agent，并明确委托它处理这些通知。然后启动采集：

```bash
state/assistant/run serve
```

新安装无需注册 Raft App，也无需域名或公网入站端口。完整的凭据取得、后台启动、验证和故障恢复步骤见 **[快速部署](docs/QUICKSTART.md)**。

## 能做什么

- 从 GitHub Notifications 自动发现公开 Issue／PR，也支持配置固定关注对象。评论、评审、时间线及当前 head 的检查会完整翻页读取；响应过大时显式报错。
- 快照和待发事件一起持久化。发送失败保留待发状态并退避重试；完整消息请求在首次发送前保存，重启后使用相同幂等键和内容重试。
- 执行 Agent 读取事件前后差异、最新快照和既有处理记录，避免把重复通知当作新工作。Raft 接受消息与 Agent 完成处理分别记账。
- 配置可信的仓库指引后，执行 Agent 可继续已授权的本地调查、独立 worktree 修改和验证。未配置的仓库默认只读，程序不提供 GitHub 写接口。

## 运维与开发

| 需要做什么 | 入口 |
| --- | --- |
| 安装、配置、常驻运行、备份和恢复 | [快速部署](docs/QUICKSTART.md) |
| 选择数据库、配置云数据库连接、扩展持久化实现 | [存储配置与实现](docs/STORAGE.md) |
| 让执行 Agent 处理通知和保存结果 | [Agent 工作说明](docs/AGENT-WORKFLOW.md) |
| 仓库操作范围与本地工作命令 | [仓库工作指引](docs/REPOSITORY-WORK.md) |
| 已有 App 部署的 OAuth／HTTPS 接入 | [App 模式参考](docs/APP-MODE.md) |
| 本次验证的范围和限制 | [部署验证](docs/SETUP-VALIDATION.md) |

```bash
make check build test
# 默认测试使用临时 SQLite；服务器测试会清空指定测试库的 ghraft_* 表。
GHRAFT_TEST_DRIVER=postgres GHRAFT_TEST_DSN_FILE=/private/path/test-dsn make integration
# 本地 SQLite + 模拟 GitHub/Raft，验证首次安装，不需要 Docker。
python3 scripts/smoke-setup.py
# 单独验证新建 MySQL 容器，结束时删除本次测试卷。
python3 scripts/smoke-setup.py --database mysql
```

当前支持单用户、单执行 Agent、单机部署。默认每 120 秒查询 GitHub，尚未实现 Webhook、私有仓库采集、多租户服务或跨主机执行。轮询快照无法保证还原采集间隔内出现后又被删除的内容；幂等写也不能替代执行 Agent 的重复事件检查。

External Agent 接口按官方 SDK 0.30.0 的契约实现。协议模拟测试不等于真实账号完成登录、收信和处理；部署者应完成快速部署中的实际接收验证。

## 来源

- [Raft External Agents](https://docs.raft.build/developers/sdk/external-agents/)
- [Raft SDK API Client](https://docs.raft.build/developers/sdk/api-client/)
- [GitHub CLI](https://cli.github.com/)
- [Docker Compose 安装](https://docs.docker.com/compose/install/linux/)
