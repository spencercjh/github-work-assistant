# 自助部署验证

验证日期：2026-10-24。本页记录 External Agent 自助部署与随后增加的数据库选择功能。已有 App 模式另行保留。

## 数据库选择验证

当前业务存储接口由 GORM 实现，默认安装使用本地 SQLite，可选择 MySQL、seekdb、TiDB 或 PostgreSQL 连接。SQLite、MySQL 8.4、PostgreSQL 18、seekdb 1.4.0.0 均通过同一套完整 race 测试，包括两个独立连接竞争首次写入、不同任务绑定冲突、待发事件写入失败后的事务回滚，以及重启去重和过期证据拒绝。

从空目录分别验证了默认 SQLite、新建 MySQL 容器、已有 PostgreSQL 三种安装方式：初始化、迁移、只读诊断、认证失败、收集投递、处理记账、重启与重复实例保护均通过。SQLite 诊断不会创建缺失的数据库。编译入口关闭 CGO，SQLite 使用纯 Go 驱动。

CI 分别覆盖 SQLite、MySQL 和 PostgreSQL；seekdb 在独立本机实例验证。TiDB 驱动入口沿用 MySQL，尚未在真实 TiDB 或 TiDB Cloud 验收。数据库位于云端时，网络、TLS、账号权限和备份恢复仍由部署者验证；没有自动跨后端数据迁移功能。详见[存储配置](STORAGE.md)。

当前模块要求 Go 1.26+。下列记录保留首次部署改造时的验证范围。

## 已验证

| 路径 | 验证方式与结果 |
| --- | --- |
| 首次安装 | `scripts/smoke-setup.py` 从新目录执行真实 `ghraft init`，启动独立 MySQL 8.4，完成迁移、诊断、自动发现、投递和本地处理记账；通过。GitHub 与 Raft 使用本机受控 fixture。 |
| 凭据和身份 | 初始化验证 External 身份与 read/send 能力；托管身份、不同 Server、能力不足、错误凭据、公开权限文件和符号链接被拒绝。诊断不发送消息。 |
| 安装文件 | 配置和秘密使用私有权限，重复初始化被拒绝且不改变原密钥；带空格、引号和特殊字符的路径覆盖到启动入口验证。生成的 systemd 文件经本机解析检查。 |
| 持久化发送 | 真实数据库测试模拟远端已接收但响应丢失；重开 Store、改变本地构建位置后仍重放相同完整请求。`held`、`not_found` 和错误回执不计作已接受，更换收件人拒绝发送旧积压。 |
| 处理记录 | 新进程不再次投递已接受事件；接受后仍列为待处理，保存 quiet／notified 判断后退出待处理列表。错误通知目标及撤销访问的资源不能记账。 |
| 进程生命周期 | 独立安装的采集进程可启动并接收终止信号正常退出；第二个本机实例被文件锁拒绝。测试产生的数据库、卷和临时凭据已清理。 |
| 原有流程 | 实际 seekdb 1.4 测试库运行完整 race 集成套件；覆盖 App 模式身份撤销、积压处理、真实本地 Git worktree、版本漂移与报告恢复。 |
| 开发检查 | `go vet`、格式与差异检查、Bash 语法检查、构建和 race 测试通过。新增 CI 使用固定的 MySQL 镜像和 Actions 提交，运行集成与首次安装验证。 |

MySQL 容器使用 `mysql:8.4@sha256:76f0358884cb92d9a086d24e5671c38048d3fef40f2588703d73eb55392d6b69`。本机 Compose 使用官方 v5.6.1 Linux 二进制并核对发布摘要。首次部署改造的 Go 构建环境为 1.26.5，当时模块要求最低 1.24；数据库改造后最低要求已更新为 1.26。

## 尚需实际部署验证

本次没有创建新的真实 External Agent、使用他人的 Raft 凭据或向真实收件人发送测试事件。部署者仍须按[快速部署](QUICKSTART.md)验证自己的 token、执行 Agent 可达性、同机 CLI 访问和真实处理记录。

本次没有替换已有运行中的服务，也没有执行机器重启或无登录启动测试。生成服务文件和进程启停测试不能代替目标机器的 systemd／Docker 开机验证。数据库备份恢复命令已提供，跨版本回滚兼容性仍需在选定版本之间单独验证。

GitHub 数据源仍为定时采集，未实现 Webhook、速率限制自适应、私有仓库或全量历史归档。测试证明被覆盖的持久化和重试路径，不构成长时间运行绝不漏报或永久恰好一次的保证。

## 协议来源

External 模式按 [Raft API Client](https://docs.raft.build/developers/sdk/api-client/) 与 `@botiverse/raft-sdk` 0.30.0 的 Agent API 契约实现，使用 context 身份读取和带幂等键的 send。参考 [External Agents](https://docs.raft.build/developers/sdk/external-agents/) 了解账号接入。

systemd 文件按官方 [命令行规则](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html#Command%20lines) 和 [环境变量规则](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#Environment=) 分别处理命令参数、路径与环境变量中的特殊字符。
