# 部署自己的 GitHub 助手

本流程面向 Linux 单机部署。采集程序与 Raft 执行 Agent 使用同一台电脑、同一个操作系统账号。程序以独立 External Agent 身份发送变化通知；执行 Agent 负责理解内容和与你协作。

## 准备账号与工具

- 在电脑上安装并运行 Raft，准备一个可用的执行 Agent，例如 `@reviewer`。确认该 Agent 能运行本机命令；它需要读取本次安装目录。
- 在同一个 Raft Server 的 **External Agents** 页面创建另一个身份，用于运行采集程序，并取得其登录 token。它需要 `read` 和 `send` 能力。不要使用执行 Agent 的管理凭据。页面入口与登录方式见 [Raft 官方说明](https://docs.raft.build/developers/sdk/external-agents/)。
- 安装 [Go 1.24 或更新版本](https://go.dev/doc/install)、[GitHub CLI](https://cli.github.com/)、[Docker Engine](https://docs.docker.com/engine/install/) 和 [Docker Compose 插件](https://docs.docker.com/compose/install/linux/)。服务账号必须有权运行 Docker；先检查 `docker info` 和 `docker compose version`。
- 以服务账号运行 `gh auth login --hostname github.com`。Notifications 读取需要对应权限；使用个人访问令牌时须支持 Notifications API，并有目标资源读取权限。程序不提取或复制 `gh` 的凭据。

无需新增模型 API key、Raft App 注册、回调地址或公网入站端口。External Agent 在这里是发送通知的程序身份，不运行模型，也不处理发给它的聊天。请继续与执行 Agent 对话。

## 安装

先取得仓库访问权限，再运行：

```bash
gh repo clone spencercjh/github-work-assistant
cd github-work-assistant
make setup
```

依次填写执行 Agent 名称、可选的人类通知收件人、External Agent token。token 输入不会回显。向导验证 GitHub 登录与 External Agent 身份，并自动填入账号和 Server ID。

安装产生私有目录 `state/assistant`：

| 文件 | 用途 |
| --- | --- |
| `config.json` | 账号、收件人、关注对象和轮询配置 |
| `run` | 固定使用本实例配置、密钥和二进制的启动入口 |
| `database.yml` | 独立的 MySQL 8.4 Compose 配置，镜像固定到摘要 |
| `private/` | 数据库密码、DSN、加密密钥和 External Agent token |
| `EXECUTOR.md` | 交给执行 Agent 的本地操作说明 |
| `ghraft-*.service` | 已填写本机路径的 systemd 用户服务 |

MySQL 只绑定 `127.0.0.1:13306`，数据保存在专用 Docker 卷。配置、凭据文件权限为 `0600`，目录为 `0700`。运行入口绑定此实例，不使用其他安装遗留的 `GHRAFT_*` 环境变量。不要把 `state/`、凭据或数据库备份提交到 Git。

若端口已占用，或需要自动化安装，先把 External Agent token 写入一个 `0600` 普通文件，再运行：

```bash
make build
bin/ghraft init --dir state/my-assistant \
  --raft-token-file /private/path/external-token \
  --recipient @reviewer --notify @owner --db-port 13307

docker compose -f state/my-assistant/database.yml up -d --wait
state/my-assistant/run migrate
state/my-assistant/run doctor
```

`--notify` 可省略。`--watch 'owner/repo#123,owner/other#456'` 可增加固定关注对象。`--raft-api` 仅用于指定其他 Raft HTTPS API 入口；程序拒绝重定向。不要把 token 放在命令参数、环境变量值或聊天中。

`init` 不覆盖已有目录。若数据库下载或启动失败，修复原因后从 `docker compose … up -d --wait` 继续，再执行 `migrate` 和 `doctor`，无需重新生成身份和密码。

## 配置执行 Agent 与关注范围

把 `state/assistant/EXECUTOR.md` 的绝对路径发给执行 Agent，明确委托它读取并处理本安装发来的 GitHub 通知。发送方是刚创建的 External Agent，执行方是 `config.json` 的 `delivery_target`。使用稳定名称，不能把发送方填成自己的收件人。

初始安装通过 GitHub Notifications 自动发现公开 Issue／PR。首次发现的事项保留最近 6 小时内的变化供判断，后续沿持久化游标继续；它不是 GitHub 全量历史归档。可在 `config.json` 增加固定关注对象：

```json
"watches": [{"repo": "your-name/your-repo", "number": 123}]
```

自动发现由 `discover_notifications` 控制；关闭时必须保留至少一个固定关注对象。默认 `poll_seconds=120`，最小 60 秒。程序尚未根据 GitHub `X-Poll-Interval` 动态调整周期，关注对象较多时需结合账号限额增加间隔。

需要代码工作时，按[仓库工作指引](REPOSITORY-WORK.md)在本实例的 `repositories` 中配置目标、可信指引和测试命令。默认仅调查；本地修改权限与 GitHub 发布权限分别取得授权。`Project-HAMi` 仓库始终限制为只读。

配置在进程启动时读取。修改配置后先执行 `run check-config`，再重启采集进程。不要直接更换现有安装的发送身份或执行收件人：待发请求已绑定原身份和目标。确需更换时，先处理旧积压并停止旧实例，再建立新实例；新实例会重新观察关注对象。

## 验证实际收信

```bash
state/assistant/run doctor
state/assistant/run collect
state/assistant/run events
state/assistant/run deliver
state/assistant/run status
state/assistant/run pending
```

`doctor` 只检查 GitHub 登录、数据库、表结构以及 External Agent 的身份和能力，不发送测试消息，也不证明收件人可达。若没有近期公开 Notifications，请增加一个真实的固定关注对象后再执行 `collect`；首次观察会产生事件。

检查以下结果后再判断助手可用：

- `status` 中关注对象有新的 `last_success`，没有未解释的采集错误。
- 执行 Agent 的 Raft 会话收到了对应事件 ID。`events` 中的 `accepted` 仅表示 Raft 接受消息。
- 执行 Agent 能读取 `work-context`、给出有来源的判断，并用 `record-triage` 或 `record-work-result` 保存记录。处理后对应事件不再出现在 `pending`。

发送失败保持 `pending` 并按退避重试，最多间隔一小时。重启不会强行跳过退避。完整请求在第一次发送前已保存；响应丢失后重试使用同一幂等键和内容。服务端去重记录有保留期限，因此执行 Agent 仍须按事件 ID 检查处理历史。

此程序发送独立的事件通知，使用 Agent API 的 `continueAnyway`，不读取用户聊天以解除发送暂停，也不按聊天上下文自动生成回复。若服务端仍返回 `held`，程序会报告错误并保留事件；应在 Raft 检查该身份和会话，不能据此标记已送达。

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

生成的服务保存初始化时的可执行文件路径和 `PATH`。移动仓库或工具后应更新服务与 `run` 中的路径。`enable` 保证用户服务管理器启动后拉起服务；若要求退出登录后或开机无登录运行，需由机器管理员启用该账号的 linger，并确认 Docker 随机器启动。

每套数据库只能有一个采集实例。本机锁防止重复启动同一实例，但不能防止另一台机器连接同一数据库。External 模式不监听 HTTP；通过 `doctor`、`status` 和服务日志检查运行情况。

## 备份、升级与恢复

数据库卷保存快照、待发事件和处理记录；实例目录保存配置和密钥。两者必须配套备份，`private/key` 丢失会使数据库中的加密状态无法解密。备份也应保持私有权限。

暂停采集服务后，可用生成的 Compose 配置导出本实例数据库：

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

恢复到原安装位置时，先恢复私有配置，启动该 Compose 数据库，再导入对应备份：

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
| `doctor` 成功但没有消息 | 检查是否有关注对象、`collect` 是否成功、`events` 的状态与退避时间，再核对执行 Agent 名称。 |
| 消息已送达但 `pending` 仍有记录 | 执行 Agent 尚未记录判断或报告；投递和处理是两件事。 |
| `another local service owns this instance` | 找到已有服务，使用已有进程；不要同时运行前台和 systemd 实例。 |
| 初次建库失败 | 检查 Docker、磁盘空间与端口，再从 Compose 启动步骤继续；不要删除已有实例来绕过错误。 |

已有 App 模式部署继续参考 [App 模式文档](APP-MODE.md)。本向导不会迁移它的数据库、更新正在运行的服务或改变原来的授权。
