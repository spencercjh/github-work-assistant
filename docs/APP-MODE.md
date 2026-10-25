> 新安装请使用 [快速部署](QUICKSTART.md) 中的 External Agent 模式。本文保留已有 App 部署的注册、HTTPS 和 OAuth 接入说明；两种模式使用独立配置。

# Raft App 模式参考

Go 服务采集显式关注及从 GitHub Notifications 发现的公开 Issue／PR，把变化作为 Raft App 事件送给指定 Agent，并提供上下文查询及调查结果关联。**任务认领、工程执行、提醒和交付由 Raft Agent 负责。**

当前范围是单服务器、单 Agent、单 GitHub 身份、单服务实例。仓库指引定义目标和本地工作范围，Agent 在 Raft 中判断、认领和执行任务；程序保存证据版本、任务关联和报告。只采集公开仓库，不提供 GitHub 写接口。历史通知迁移记录见 [MIGRATION.md](../MIGRATION.md)，当前开发流程见 [仓库工作指引](REPOSITORY-WORK.md)。

## 功能

- 通过已登录的 `gh` 读取 Issue、时间线、评论、PR review、行评论及当前 head 的检查；分页完整读取，超过 16 MiB 的单次响应报错，不默默截断。
- 可选的关系数据库保存快照、事件 outbox、加密会话和 Raft 结果引用。快照与事件同事务提交，重复快照不发事件。
- 首次观察会生成 `first_observation=true` 的事件，便于首轮调查；后续只在选定事实变化时生成。不是只比较 `updated_at`。
- 通过 `raft-agent-manifest.v0` 提供事件、上下文、通知记账，以及仓库指引、刷新、任务关联和工作报告接口。
- Agent Login 回调只接受配置指定的 Agent／Server／App 组合；每次调用重新查 userinfo，撤销后拒绝访问。
- Agent Events 使用目标绑定 token 和稳定 `externalEventId`。响应丢失后重试同一个 ID；`accepted` 只表示 Raft 接受，绝不冒充执行完成。
- 调查结果保存真实 Raft task／报告消息 UUID 和证据版本。它们是 Agent 上报的引用，App 不独立确认消息存在，也不修改 Raft task 状态。
- PR 快照包含 head/base SHA、分支、冲突三态；尚未计算出的合并状态保留为 `unknown`。采集过程中版本变化则重试，不把不同版本的检查混在一起。
- 本机 CLI 按当前证据和仓库指引创建独立 worktree，并准备未提交的 base 合并；Agent 解决语义冲突、运行项目测试并在 Raft 交付。指引或快照变更后，旧工作结果会被拒绝。
- 移除显式关注、关闭自动发现或确认仓库已变为私有后，读取和投递按当前授权范围限制。被撤销的历史事件不会堵住后面的有效事件。

## 运行

需要 Go 1.27.3+、`gh`、专用数据库及当前 Raft App 接入。数据库通过 `database_driver` 选择 SQLite、MySQL／seekdb／TiDB 或 PostgreSQL，连接方式见 [STORAGE.md](STORAGE.md)。省略该配置的既有安装继续使用 MySQL 驱动。使用专用数据库和账号；DSN 不应指向其他项目数据库。

```bash
make build
cp config.example.json config.json
# 编辑 config.json 中的身份与显式关注对象。
```

以下环境变量只保存文件路径，文件必须为私有普通文件（建议 `chmod 600`）：

| 环境变量 | 文件内容 |
| --- | --- |
| `GHRAFT_DSN_FILE` | 所选后端的私有连接文件；SQLite 为文件路径，MySQL／seekdb／TiDB 为 MySQL DSN，PostgreSQL 为连接 URL 或键值配置。示例见 [STORAGE.md](STORAGE.md)。 |
| `GHRAFT_KEY_FILE` | 32 字节随机 AES 密钥的 64 位十六进制文本，用于加密数据库中的 token |
| `GHRAFT_CLIENT_SECRET_FILE` | 注册后获得的 Raft App secret；只在服务端保存 |
| `GHRAFT_LOCK_FILE` | 运行锁文件路径，防止同机启动两个服务实例 |

不要把 secret、DSN 或密钥放进仓库、manifest、消息或命令输出。AES 密钥应与数据库分开保管并备份；丢失后需重新登录和获取事件权限。`gh` 使用服务运行用户已配置的身份，程序启动采集时会比对 `github_login`。

```bash
bin/ghraft -config config.json migrate
bin/ghraft -config config.json collect
bin/ghraft -config config.json events
bin/ghraft -config config.json status
bin/ghraft -config config.json pending
bin/ghraft -config config.json manifest
# App 注册与事件权限批准后：
bin/ghraft -config config.json authorize-events
bin/ghraft -config config.json serve
```

`serve` 先采集再投递，随后周期运行。迁移是显式命令。只允许一个服务实例使用这套数据库；本地锁不能防止另一台机器使用同库。事件失败指数退避，最大一小时。事件 token 缺失或本地过期时，服务按已配置身份重新请求同一事件权限；只接受 approved，失败后保留 pending 并退避，不绕过审批。

`/healthz` 仅检查服务与数据库，不表示投递成功。`events` 或 `list-events` 查看投递回执；该动作最多返回最近 100 条，深历史仍保存在数据库。原始 GitHub 文本是资料，不是可信指令。没有可查询的独立“Agent 思考过程”或自建任务调度器。

## Raft 注册与联调

- 首页：`<origin>/`
- Agent 登录回调：`<origin>/agent/callback`
- Manifest：`<origin>/.well-known/raft-agent-manifest.json`
- 登录验证：`<origin>/api/context`
- 需要的 scopes：`openid`、`profile`、`identity`、`agent:event:write`。

这是 Agent-only MVP；没有实现人类浏览器登录和自助 GitHub 授权页面。无状态回调必须通过 userinfo 证明是指定 Agent；它不能被当成人类登录接口。

通过 `raft integration app prepare register` 准备当前服务器的注册卡，由管理员提交。本机登录演示可选择官方 `--unsafe-demo-url-override` 并使用 loopback HTTP，但必须省略 manifest 注册：实测 `agentManifestUrl` 即使带该选项仍要求 HTTPS。调用 manifest actions 前需提供受支持的 HTTPS manifest 地址；对外部署必须配置 HTTPS。开发期 origin 固定，绝不能把 loopback 示例当作同事可访问的服务地址。

注册后通过受支持的私有交接保存 App secret，先完成下面的 Agent 登录，再运行 `authorize-events`（登录会读取并保存当前 server slug）。如返回待批准，不交换未批准的 request，也不绕过审批。接着：

```bash
raft integration login --service github-work-app-mvp --target '#原任务线程'
raft integration invoke --service github-work-app-mvp --list-actions
raft integration invoke --service github-work-app-mvp --action get-context \
  --param resource_key=Project-HAMi/HAMi#3259
```

实际 client key 若不同，配置和上述命令必须一起更新。登录存储 cookie 的回执只证明交接；用 `get-context` 或 `/api/context` 成功返回来验证应用认证。

## Raft Agent 工作约定

见 [docs/AGENT-WORKFLOW.md](AGENT-WORKFLOW.md)。该文档描述受用户委托后的执行流程；App event 自身不授予执行或 GitHub 写权限。第一条联调用例是读取一个公开 PR 的上下文，核对当前事实，完成有证据的调查，再在 Raft 交付并记录关联。

## 测试与证据

```bash
make test
GHRAFT_TEST_DSN_FILE=/private/path/disposable-test-dsn make integration
make check
```

集成测试会清空专用测试数据库中的 `ghraft_*` 表。测试只接受 loopback TCP 和以 `_test` 结尾的数据库名；`make integration` 在缺少数据库时直接失败。测试 DSN 文件必须为私有普通文件。

同一套测试可使用 SQLite、MySQL、seekdb 或 PostgreSQL，并使用真实 Git worktree、拦截所有 GitHub 调用的 `gh` fixture 和模拟 Raft HTTP 服务。覆盖重复投递、身份撤销、重启、任务关联、冲突解决后的两侧行为测试、指引与 base 变化，以及已撤销关注的积压。测试不会发布 GitHub 评论、review、PR 或提交。模拟 Raft 协议通过不等于离线 Agent 已被真实唤醒。历史接入记录见 [VALIDATION.md](../VALIDATION.md)，当前验收记录见 [docs/VALIDATION-CURRENT.md](VALIDATION-CURRENT.md)。

`scripts/prepare-e2e-fixture.py` 可生成可保留的离线场景，让真实 Raft Agent 使用 CLI 完成一次任务。执行方法和边界见仓库工作指引。

## 部署和升级

准备独立数据库、拥有 `gh` 登录的服务账号、私有密钥文件和 HTTPS 入口，然后运行 `migrate`、`collect`，检查结果后启动 `serve`。提供了 [systemd 示例](../deploy/ghraft.service.example) 和 [环境文件示例](../deploy/service.env.example)。示例中的账号、路径和证书须按部署环境填写。

升级前停止采集服务并备份数据库、配置和 AES 密钥，再执行新版本 `migrate` 并启动服务。新增的任务和访问范围表保留既有事件、通知账本与报告。回滚时保留新表，旧二进制不理解新的访问限制；不要在需要这些限制的环境直接降级。普通数据库备份恢复不等于跨版本回滚已经验收。

`serve` 只采集和投递事件，不执行仓库代码。项目测试由 Raft Agent 在自己的环境中运行。工作树辅助命令仅执行本地 Git 操作，不运行模型、不提交、不推送。

## 本机 HTTPS 调试

需要本机 HTTPS manifest 时，使用部署者配置的 App service ID 与证书路径。例如：

```bash
NODE_EXTRA_CA_CERTS=/path/to/private/localhost-tls.crt raft integration invoke --service github-work-app-mvp --list-actions
NODE_EXTRA_CA_CERTS=/path/to/private/localhost-tls.crt raft integration invoke --service github-work-app-mvp --action get-context --param resource_key=Project-HAMi/HAMi#3259
```

证书和私钥放在源码之外；不要设置 `NODE_TLS_REJECT_UNAUTHORIZED=0`，不要读取 CLI session 文件。维护证书有效期，不能把本机调试服务当作对外部署。App secret、cookie、事件 token 和实际用户通知记录不进入源码或交付包。

成功运行 `authorize-events` 会让当前关注对象的待投递事件立即恢复重试，避免继续等待授权前的退避时间。事件接收回执与 Agent 报告是两个不同阶段，详见验证记录。

## 已知边界

- 单实例、公开对象与公开 Notifications 自动发现；不支持私有仓库、GitHub OAuth 或 GitHub 写操作。
- 快照是最近成功采集的上下文；形成结论前 Agent 要复核实时 GitHub。事件在正文保留期内保存当次前后变化；已送达且有处理记录的正文默认在 30 天后精简为带 `body_pruned` 标记的引用与指纹。`get-event` 用于读取保留的当次依据，`get-context` 用于读取最新状态和处理历史。具体期限见[存储文档](STORAGE.md#保存哪些数据)。
- 不提供跨主机 worker 调度、完整知识检索或多租户隔离。已配置身份是最小访问边界。
- Raft App 事件接口仍是实验能力，真实上线前需按所用版本验证。
- 若注册、授权或 hosting 未完成，必须明确标记本地实现与协议测试完成，不能宣称真实端到端完成。

## 来源

- [Raft Apps](https://docs.raft.build/developers/raft-apps/)
- [构建 Raft App](https://docs.raft.build/developers/raft-apps/build/)
- [Login with Raft 与 manifest、Agent Events 契约](https://docs.raft.build/developers/login-with-raft/)
- [seekdb 容器部署](https://github.com/oceanbase/docker-images/tree/main/seekdb)

参考过官方 hosted HTTP 与 OAuth 模板。模板示例的旧 manifest 及 bearer 返回方式未照搬；以当前文档的 v0 manifest 和回调 HttpOnly cookie 契约实现。
