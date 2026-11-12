# 部署自己的 GitHub 助手

本流程面向 Linux 单机部署。采集程序与 Raft 执行 Agent 使用同一台电脑、同一个操作系统账号。程序以独立 External Agent 身份发送变化通知；执行 Agent 负责理解内容和与你协作。

## 准备账号与工具

- 在电脑上安装并运行 Raft，准备希望参与工作的执行 Agent。它们都需要能运行本机命令并读取本次安装目录。
- 在同一个 Raft Server 的 **External Agents** 页面创建另一个身份，用于运行采集程序，并取得其登录 token。它需要 `read`、`send`、`tasks` 和 `channels` 能力。不要使用执行 Agent 的管理凭据。页面入口与登录方式见 [Raft 官方说明](https://docs.raft.build/developers/sdk/external-agents/)。
- 创建一个工作频道，例如 `#github-work`，将采集身份和执行 Agent 加入。执行者由频道成员管理，无需在助手中维护名单。执行 Agent 应接收该频道的新任务通知，并获得用户的处理委托。
- 安装 [Go 1.27.3 或更新版本](https://go.dev/doc/install)和 [GitHub CLI](https://cli.github.com/)。默认使用本地 SQLite，无需数据库服务或 C 编译器。只有选择新建 MySQL 容器时，才需要 [Docker Engine](https://docs.docker.com/engine/install/) 和 [Compose 插件](https://docs.docker.com/compose/install/linux/)，并检查服务账号能运行 `docker info` 和 `docker compose version`。
- 以服务账号运行 `gh auth login --hostname github.com`。Notifications 读取需要对应权限；使用个人访问令牌时须支持 Notifications API，并有目标资源读取权限。程序不提取或复制 `gh` 的凭据。

无需新增模型 API key、Raft App 注册、回调地址或公网入站端口。External Agent 在这里是发送通知的程序身份，不运行模型，也不处理发给它的聊天。请继续与执行 Agent 对话。

## 安装

先取得仓库访问权限，再运行：

```bash
gh repo clone spencercjh/github-work-assistant
cd github-work-assistant
make setup
```

先选择本地 SQLite、新建 MySQL 容器或已有数据库，再填写工作频道、可选的人类通知收件人、External Agent token。token 输入不会回显。向导验证 GitHub 登录与 External Agent 身份，并自动填入账号和 Server ID，并检查发送方是否已加入频道。已有数据库的连接文件和云数据库配置见[存储配置](STORAGE.md)。

安装产生私有目录 `state/assistant`：

| 文件 | 用途 |
| --- | --- |
| `config.json` | 数据库类型、账号、收件人、关注对象和轮询配置 |
| `run` | 固定使用本实例配置、密钥和二进制的启动入口 |
| `database.yml` | 仅新建 MySQL 容器时生成，镜像固定到摘要 |
| `private/` | 数据库连接文件、加密密钥和 External Agent token；默认 SQLite 数据库也存于此处 |
| `EXECUTOR.md` | 交给执行 Agent 的本地操作说明 |
| `ghraft-*.service` | 已填写本机路径的 systemd 用户服务 |

默认 SQLite 保存在 `private/assistant.sqlite`。新建 MySQL 容器只绑定 `127.0.0.1:13306`，数据保存在专用 Docker 卷；已有数据库沿用提供的连接。配置、凭据和 SQLite 文件权限为 `0600`，目录为 `0700`。运行入口绑定此实例，不使用其他安装遗留的 `GHRAFT_*` 环境变量。不要把 `state/`、凭据或数据库备份提交到 Git。

自动化安装时，先把 External Agent token 写入一个 `0600` 普通文件。以下示例使用默认 SQLite：

```bash
make build
bin/ghraft init --dir state/my-assistant \
  --raft-token-file /private/path/external-token \
  --task-channel '#github-work' --decision-maker @owner --database sqlite

state/my-assistant/run migrate
state/my-assistant/run doctor
```

需要独立 MySQL 容器时，将 `--database sqlite` 改为 `--database mysql --db-port 13307`，并在 `migrate` 前执行 `docker compose -f state/my-assistant/database.yml up -d --wait`。已有数据库使用 `--database postgres --dsn-file /private/path/database-dsn` 等组合，程序不会创建数据库服务。

`--decision-maker` 必填，使用需要做决策的人的 Raft 稳定名称，例如 `@owner`，不能用 GitHub 登录名代替。共享模式会检查此人已加入工作频道。执行者只在需要决策时于原讨论提及此人；此配置不启用私聊。需要额外私聊时才添加可选的 `--notify @owner`。

`--watch 'owner/repo#123,owner/other#456'` 可增加固定关注对象。`--raft-api` 仅用于指定其他 Raft HTTPS API 入口；程序拒绝重定向。不要把 token 放在命令参数、环境变量值或聊天中。

已有实例升级时，在 `config.json` 增加 `"decision_maker": "@owner"`，核对工作频道中的人类成员，更新 `EXECUTOR.md` 中的[决策请求规则](AGENT-WORKFLOW.md#需要用户决策时)，再执行 `run check-config`、`run doctor` 并重启服务。让执行者重新读取说明。决策人变化会更新指引版本；正在工作的执行者需要重新读取 `work-context` 并关联当前版本。未配置决策人的旧实例仍可采集，但共享任务不能记录 `needs-decision`。

`init` 只生成配置，不覆盖已有目录。`migrate` 创建 SQLite 文件或初始化已选数据库的表；`doctor` 不创建缺失数据库。若初始化失败，修复路径或连接后重试 `migrate`、`doctor`。新建 MySQL 容器失败时，先重试 Compose 启动步骤，无需重新生成身份和密码。

## 升级已有的事件任务

升级前先让旧版本完成待发请求；结果不明确的请求需要先核对原投递，不能清空记录绕过检查。新版本为每个 PR/Issue 保存一个固定根任务，不重写已发送的消息。已有且唯一的共享任务会自动复用；如果同一事项已有多个历史任务，程序暂停该事项的后续投递，要求部署者明确选择。

将 `INSTANCE` 设为当前安装目录的绝对路径。使用该事项的任一事件 ID 列出候选，再根据用户决定选择已有任务的完整根消息 UUID：

```bash
"$INSTANCE/run" discussion-candidates --event "$EVENT_ID"
"$INSTANCE/run" adopt-discussion --event "$EVENT_ID" --task "$ROOT_MESSAGE_UUID"
```

候选只来自该事项在当前工作频道中的已确认投递；选择时还会核对任务仍可读取。此操作不创建任务、不改派负责人、不关闭其他历史任务，也不能替换已经固定的讨论区。选定后，旧事件的执行归属也按这个固定任务校验。需要结束历史重复任务时，由用户决定并在 Raft 处理。

## 配置任务频道与关注范围

把 `state/assistant/EXECUTOR.md` 的绝对路径交给频道中的执行 Agent，明确委托它们处理通知及允许的操作范围。配置保存工作频道和需要做决策的人：

```json
"delivery_target": "#github-work",
"decision_maker": "@owner",
"notify_target": ""
```

每个 PR/Issue 首次创建一个未分配任务，频道里的 Agent 竞争认领。后续事件追加到同一个讨论区并提醒当前负责人；任务完成不会解除归属。Raft 保证同一任务只有一个负责人；成功者持续处理，已被别人认领者静默结束，不重试争抢、不另建任务、不向用户报错。网络或权限错误需要排查，不能当成正常落选。完整操作见[执行工作流](AGENT-WORKFLOW.md#竞争认领共享任务)。

增减执行者只需管理频道成员，并向新成员提供 `EXECUTOR.md`；不修改助手名单。不要静音用于接收工作的频道。频道中的人类成员也可能拥有认领权限；应用没有额外的候选人权限表。执行者中断后任务不会自动转移，只有用户明确要求时，才由有权限的用户在原任务上指派接手者。

单人 DM 投递仍可用 `--recipient @reviewer` 安装，与 `--task-channel` 二选一。它不创建共享任务，也不提供竞争认领。

初始安装默认通过 GitHub Notifications 自动发现公开 Issue／PR。私有仓库需配置 `private_repositories`，支持组织、仓库或全部可读范围，见[私有仓库配置](PRIVATE-REPOSITORIES.md)。首次发现的事项保留最近 6 小时内的变化供判断；私有范围可另设固定起点，后续沿持久化游标继续。它不是 GitHub 全量历史归档。可在 `config.json` 增加固定关注对象：

```json
"watches": [{"repo": "your-name/your-repo", "number": 123}]
```

自动发现由 `discover_notifications` 控制；关闭时必须保留至少一个固定关注对象或 fork 维护规则。默认 `poll_seconds=120`，最小 60 秒；需要降低频率时可设为 `600`（10 分钟），重启服务后生效。每轮结束后等待该间隔，耗时较长的采集不会紧接着补跑。普通 Issue／PR 的后台轮询按状态处理：

| 状态或触发条件 | 查询行为 |
| --- | --- |
| 首次关注、仍打开 | 每轮完整刷新，包含评论、评审和当前提交的 CI。CI 更新不一定改变 Issue 的更新时间，不能仅凭该时间跳过查询。 |
| 已关闭或合并 | 停止定时轮询，不再定期读取状态、评论、评审或 CI。升级和重启后仍保持停止。 |
| 已结束事项收到新 Notifications | 下一轮完整刷新一次。重叠发现窗口里的同一条通知不重复触发；刷新失败会保留触发记录，重启后继续处理。 |
| 执行者主动刷新、操作前校验或任务收尾 | 实时完整查询，不受后台停止轮询的限制。 |
| 刷新发现事项重新打开 | 恢复正常轮询，并沿用已有工作关联。 |

已结束事项不保留小时检查或每日核对。如果重开没有产生可读取的个人 Notifications，需要主动执行 `refresh-work-context` 才能发现；关闭自动发现时，同样需要主动刷新。既有快照、未处理事件及工作关联继续保留，停止轮询不等于删除数据或自动忽略待处理事件。

持续 fork 维护规则继续轮询上游分支和未完成的同步 PR。队列中已核验合入并登记完成的批次保留原证据，不再重复读取其历史评论、评审和 CI；每次仍检查仓库权限、当前分支及已接受合并是否仍在目标历史中。仅在同一数据库、相同配置下实际重叠的 fork 刷新可共享一次成功采集；维护进度变化或采集失败时重新查询，后续独立操作也会重新查询。

遇到 GitHub 限流时，程序停止本轮剩余采集及投递，保留事件和通知触发记录，并按 `Retry-After` 或配额耗尽时的 `X-RateLimit-Reset` 等待。恢复时间按配置账号保存在数据库中，服务和临时 CLI 命令共同遵守；缺少可用恢复时间时从 60 秒开始退避，连续失败最多延长至 15 分钟。后台等待时间不短于 `poll_seconds`，写入前检查遇到限流会返回错误，不能使用旧证据继续写入。

程序尚未根据 GitHub `X-Poll-Interval` 动态调整周期，也未实现 Notifications 条件查询；打开事项较多时仍需结合账号限额增加 `poll_seconds`。

已确认送达且有处理记录的历史正文默认保留 30 天，由 `history_retention_days` 调整。过期后保留去重标识和报告引用；未解决事件、当前快照和任务关联不按此期限删除。可用 `state/assistant/run cleanup --dry-run` 预览，详见[数据保留与清理](STORAGE.md#保存哪些数据)。

需要代码工作时，按[仓库工作指引](REPOSITORY-WORK.md)在本实例的 `repositories` 中配置目标、可信指引和测试命令。默认仅调查；本地修改权限与 GitHub 发布权限分别取得授权。`Project-HAMi` 仓库始终限制为只读。

配置在进程启动时读取。修改配置后先执行 `run check-config`，再重启采集进程。不要直接更换现有安装的发送身份或投递频道／DM 目标：待发请求已绑定原身份和目标。确需更换时，先处理旧积压并停止旧实例，再建立新实例；新实例会重新观察关注对象。

## 验证实际收信

```bash
state/assistant/run doctor
state/assistant/run collect
state/assistant/run events
state/assistant/run deliver
state/assistant/run status
state/assistant/run pending
```

`doctor` 检查 GitHub 登录、数据库、表结构、External Agent 身份和能力；频道模式还检查发送方是否在频道中。它不创建测试任务，也不证明执行 Agent 在线或已获得委托。若没有近期公开 Notifications，请增加一个真实的固定关注对象后再执行 `collect`；首次观察会产生事件。

检查以下结果后再判断助手可用：

- `status` 中仍打开的关注对象有新的 `last_success`，没有未解释的采集错误。已关闭或合并的事项停止定时轮询，`last_success` 不再定期更新。
- 首次关注的 PR/Issue 出现一个带事件 ID 的未分配任务，后续变化追加到该任务的讨论区。`events` 中的 `accepted` 表示 Raft 接受了本次任务创建或追加消息；回执包含固定任务的频道、编号和根消息标识，追加消息另有 `notificationMessageId`。
- 成功认领者通过 `verify-claim` 后能读取 `work-context`、给出有来源的判断，并用 `record-triage` 或 `record-work-result` 保存记录。处理后对应事件不再出现在 `pending`。

发送失败保持 `pending` 并按退避重试，最多间隔一小时。重启不会强行跳过退避。完整请求在第一次发送前已保存；响应丢失后重试使用同一幂等键和内容。Raft 的写入去重键有效期为 24 小时。若结果不明确持续 23 小时，助手保留待发状态并停止自动发送，避免期限过后产生重复任务或消息；此时需检查原任务和投递记录，不能删除本机记录或换键重发来绕过检查。执行 Agent 仍须按事件 ID 检查处理历史。

追加消息和单人 DM 通知都从已保存的待发请求发送，使用 Agent API 的 `sendDraft` 与 `continueAnyway`。程序不读取用户聊天以解除发送暂停，也不按聊天上下文自动生成回复。若服务端仍返回 `held`，程序会报告错误并保留事件；应在 Raft 检查该身份和会话，不能据此标记已送达。

## 常驻运行

先用前台方式观察一轮：

```bash
state/assistant/run serve
```

确认收集和投递正常后按 Ctrl-C 停止，安装生成的用户服务：

```bash
mkdir -p ~/.config/systemd/user
install -m 600 state/assistant/ghraft-*.service ~/.config/systemd/user/
unit=$(basename state/assistant/ghraft-*.service)
systemctl --user daemon-reload
systemctl --user enable --now "$unit"
journalctl --user -u "$unit" -n 50 --no-pager
```

生成的服务保存初始化时的可执行文件路径和 `PATH`。移动仓库或工具后应更新服务与 `run` 中的路径。`enable` 保证用户服务管理器启动后拉起服务；若要求退出登录后或开机无登录运行，需由机器管理员启用该账号的 linger；选择 MySQL 容器时，还应确认 Docker 随机器启动。

每套数据库只能有一个采集实例。本机锁防止重复启动同一实例，但不能防止另一台机器连接同一数据库。External 模式不监听 HTTP；通过 `doctor`、`status` 和服务日志检查运行情况。

## 备份、升级与恢复

所选数据库保存快照、待发事件和处理记录；实例目录保存配置和密钥。两者必须配套备份，`private/key` 丢失会使数据库中的加密状态无法解密。备份也应保持私有权限。

先暂停采集服务及使用此实例 CLI 的执行操作。SQLite 默认安装可以在所有连接退出后，归档完整实例目录；使用自定义路径时，也须一起备份该路径的数据库及尚存的 `-wal`、`-shm` 文件。不要在运行中只复制 SQLite 主文件。外部 PostgreSQL 使用 `pg_dump`，外部 MySQL／seekdb／TiDB 使用服务商支持的一致性备份方式；具体要求见[存储配置](STORAGE.md)。

以下导出与导入命令仅适用于向导创建的 MySQL 容器：

```bash
systemctl --user stop "$unit"
umask 077
mkdir -p backups
docker compose -f state/assistant/database.yml exec -T database \
  sh -c 'MYSQL_PWD="$MYSQL_PASSWORD" mysqldump --no-tablespaces --single-transaction -u "$MYSQL_USER" "$MYSQL_DATABASE"' \
  > backups/assistant.sql
tar -czf backups/assistant-config.tgz -C state assistant
```

升级代码前保留旧二进制、数据库备份和实例配置。更新仓库并运行 `make build`，保留原实例目录，然后执行 `run migrate`、`run doctor` 并重启服务；不要重新运行 `init`，不要删除 Docker 卷。不同版本的数据库回滚必须先核对兼容性，不能只换回旧二进制。

SQLite 恢复时，先保持所有连接关闭，再恢复匹配的实例目录和数据库文件，检查私有权限及路径，最后执行 `run doctor`。对于向导创建的 MySQL 容器，先恢复私有配置，启动该 Compose 数据库，再导入对应备份：

```bash
docker compose -f state/assistant/database.yml up -d --wait
docker compose -f state/assistant/database.yml exec -T database \
  sh -c 'MYSQL_PWD="$MYSQL_PASSWORD" mysql -u "$MYSQL_USER" "$MYSQL_DATABASE"' \
  < backups/assistant.sql
state/assistant/run doctor
```

导入会改变目标库，应只对这份备份对应的空库或明确要恢复的实例执行。换机器时需同时修正生成的绝对路径，并保证执行 Agent 能访问新目录。旧待发请求中的工作说明路径已冻结；恢复到不同位置时保留原路径的可访问入口，或先完成旧积压再切换。

更换 External Agent token 时保持同一身份和 Server，把新 token 写入私有普通文件并替换 `private/raft-token`，然后重启并执行 `doctor`。不要在聊天或终端输出中回显 token。撤销的凭据会使投递保留待发，换成别的身份不会绕过绑定检查。

## 常见问题

| 现象 | 检查与处理 |
| --- | --- |
| `gh login does not match` 或 GitHub 查询失败 | 在运行服务的账号下检查 `gh auth status` 和 Notifications 权限，确认配置中的登录名；程序不会自动切换账号。 |
| `secret must be a private regular file` | 检查文件存在、是普通文件且权限为 `0600`；不接受符号链接。 |
| `external credential does not match` | 使用创建本实例时的 External Agent 身份；不要混用其他 Server、发送方或受管 Agent 凭据。 |
| `Raft HTTP 401/403` | 检查 token 是否撤销、能力范围及发送目标权限；事件会保留，不代表丢失。 |
| `Raft transport failed` | 检查网络和系统 CA。自建 CA 可通过 Go 的 `SSL_CERT_FILE` 提供受信任根证书，不能关闭 TLS 验证。 |
| `doctor` 成功但没有消息 | 检查是否有关注对象、`collect` 是否成功、`events` 的状态与退避时间，再核对频道成员与执行 Agent 的收信设置。 |
| 认领结果显示已被别人认领 | 这是正常结果，静默结束本次工作，不重试、不另建任务、不报错。 |
| `verify-claim` 返回 `execute=false` | 任务归别人或已进入审阅／结束，停止执行；HTTP、权限等错误不会被转成这个结果。 |
| 消息已送达但 `pending` 仍有记录 | 执行 Agent 尚未记录判断或报告；投递和处理是两件事。 |
| `another local service owns this instance` | 找到已有服务，使用已有进程；不要同时运行前台和 systemd 实例。 |
| 初次建库失败 | SQLite 检查文件路径、权限和磁盘空间；服务器数据库检查连接、TLS 和建表权限；新建 MySQL 容器另查 Docker 与端口。修复后重新执行 `migrate`，不要删除已有数据来绕过错误。 |

已有 App 模式部署继续参考 [App 模式文档](APP-MODE.md)。本向导不会迁移它的数据库、更新正在运行的服务或改变原来的授权。

## 委托自主处理 PR

安装默认只读。需要执行者持续完成 PR 检查和合入时，在安装目录的 `config.json` 中配置 `repositories`、`github_actions` 和可信的本地指引，然后重启服务。动作列表和指引共同表达用户的长期委托，范围内不需要逐次确认。示例与合入前检查见[仓库工作指引](REPOSITORY-WORK.md)。已有事项可用 `request-work` 请求重新评估；不要清空历史或通知游标。
