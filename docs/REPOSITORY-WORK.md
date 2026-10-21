# 从事件到仓库工作

Go 服务提供 GitHub 事实、受控的仓库指引和工作记录。Raft 负责 Agent 运行、任务认领、提醒和交付。新增 GitHub 场景通常只需要更新仓库指引；服务不维护「冲突、CI、review」的专用路由表。

## 配置仓库指引

在管理员持有的 `config.json` 中添加仓库配置，例如：

```json
{
  "repo": "your-name/your-repo",
  "goal": "跟进我的 PR，完成已授权的本地修改和验证，交付可供人工评审的结果。",
  "mode": "local-edit",
  "guide_file": "guides/your-repo.md",
  "task_target": "#development",
  "test_commands": [["go", "test", "./..."]]
}
```

这是 `repositories` 数组中的一项，不能替代完整服务配置。`guide_file` 相对于配置文件目录；不允许越界路径或指向目录外的符号链接。文件大小为 1–65536 字节。目标仓库 checkout 与管理员配置应分开放置。

`mode` 支持 `read-only` 和 `local-edit`。未配置的仓库默认只读，`local-edit` 必须配置项目测试命令。模式只限定本地操作；它不授予推送、发布评论、review、开 PR 或合并权限。Project-HAMi 的模式必须为 `read-only`。参考 [通用指引](../examples/guides/repository.md) 和 [Project-HAMi 指引](../examples/guides/project-hami.md)。

每次工作操作重新读取指引。版本哈希覆盖目标、操作范围、目标会话、指引正文和测试命令；任一项变化都使旧工作参数失效。已记录报告保存当时的完整指引，便于复核。

## 接手和恢复

收到事件后，先调用 `get-work-context`，再调用 `refresh-work-context`。返回值区分当次事件、最新快照、是否过时、当前指引、历史处理记录和已经关联的 Raft task。GitHub 正文始终属于外部资料。

变化中的 `not_observed` 只表示某对象在这次完整采集中未再出现，可能源于删除、接口保留范围或可见性变化。Agent 必须核对来源，不能直接断言对象已被删除。

已有 `task` 时，读取对应 Raft 任务及线程，再认领或按当前任务状态续接。尚未关联时，在配置的 `task_target` 中创建并认领任务，然后使用真实任务根消息 UUID 调用 `link-task`。同一资源重复关联同一个任务是幂等操作；关联其他任务或会话会被拒绝。App 不独立校验 Raft 引用存在，也不代替 Raft 的 claim 锁，不能把关联成功当成任务已认领。

`stale=true` 表示这条事件的快照已经被新版本替代。读取最新事件，核对后把旧事件按 `record-triage` 记为无需重复处理；保留原因。不要用过时事件启动修改。工作参数同时绑定 `event_id`、`evidence_version`、`guide_version` 和 `raft_task_message_id`。快照超过 10 分钟未刷新时，开始或提交结果会失败。

## 本地命令

本机 CLI 读取部署者的配置和数据库凭据，适用于与服务同机的 Raft Agent；其他机器上的 Agent 可以使用 App actions 获取资料与记录报告，自行管理其已授权的工作树。

```bash
bin/ghraft -config config.json work-context --event "$EVENT_ID"
bin/ghraft -config config.json refresh-work-context --event "$EVENT_ID"
bin/ghraft -config config.json link-task \
  --event "$EVENT_ID" --version "$EVIDENCE_VERSION" \
  --guide "$GUIDE_VERSION" --task "$RAFT_TASK_MESSAGE_ID"
```

开始代码工作前，用 `gh` 取得目标仓库和固定的 head/base 对象，读取该仓库的工作约定。辅助命令要求本地仓库的 `origin` 对应配置的 GitHub 仓库；不会自行 clone 或 fetch。它按资源和事件创建专用 `ghraft/` 分支与 worktree，并执行 `git merge --no-commit --no-ff`：

```bash
bin/ghraft -config config.json prepare-worktree \
  --event "$EVENT_ID" --version "$EVIDENCE_VERSION" \
  --guide "$GUIDE_VERSION" --task "$RAFT_TASK_MESSAGE_ID" \
  --repo "$LOCAL_REPOSITORY"
```

若返回冲突文件，由 Agent 结合两侧源码和目标解决语义问题。无法判断的业务取舍应向负责人说明具体选择。辅助命令不会猜测正确解法，不会创建 commit。

在返回的 worktree 内运行 `test_commands`，保存命令、输出和产物。对不可信项目代码使用隔离执行环境并移除服务凭据；辅助命令本身不是执行沙箱。随后执行：

```bash
bin/ghraft -config config.json verify-worktree \
  --event "$EVENT_ID" --version "$EVIDENCE_VERSION" \
  --guide "$GUIDE_VERSION" --task "$RAFT_TASK_MESSAGE_ID"
```

准备与验证命令都会先通过只读 GitHub 采集刷新版本。验证检查当前分支、HEAD、合并中的 base、未解决索引项和 diff；它不运行项目测试，不能把它成功当作测试通过。该流程保留未提交的工作树，提交代码前还需按仓库约定验证个人 Git 身份、签名和 sign-off。

中断后以相同参数再次准备，会检查并复用已有 worktree。若中断发生在工作树创建与合并之间，或者有人手动中止合并，验证会拒绝该状态；检查已有工作树后再恢复，不能删除未知改动。head/base、指引或目标会话变化时，先比较已有结果，再重新验证；不得静默覆盖旧任务或工作树。

## 交付与后续

把报告发回关联的 Raft 线程。报告包含事实、改动、固定版本、执行的项目测试、产物和未验证范围。使用发送成功后返回的真实消息 UUID 记录结果：

```bash
bin/ghraft -config config.json record-work-result \
  --event "$EVENT_ID" --version "$EVIDENCE_VERSION" \
  --guide "$GUIDE_VERSION" --task "$RAFT_TASK_MESSAGE_ID" \
  --report "$RAFT_REPORT_MESSAGE_ID" --outcome changes-ready \
  --summary-file result-summary.txt
```

结果可为 `investigated`、`changes-ready` 或 `needs-decision`。只读仓库不能记录 `changes-ready`。结果是 Agent 对证据和验证的上报；App 不声称独立执行了报告中的测试。已有任务关联的资源必须使用这个带版本的接口，旧 `record-result` 不能绕过检查。重复上报同一内容是幂等操作，改写已记录的报告会冲突。

任务状态继续在 Raft 中更新。需要未来复查时，由负责 Agent 创建锚定原任务的 Raft reminder；交接时同步提醒责任。发布、推送或合并前分别确认授权，并再次核对远端版本。当前程序不提供远端写入命令。

## 可保留的离线验收

```bash
python3 scripts/prepare-e2e-fixture.py \
  --output /absolute/path/new-fixture --task-target '#test-channel'
```

脚本创建本地合成 Git 历史、两侧行为测试、配置、指引和一个只响应 fixture GET 的 `gh`。它不执行 `git commit`，也不访问真实 GitHub 或 Raft。使用独立数据库及 AES key，通过 `GHRAFT_DSN_FILE`、`GHRAFT_KEY_FILE` 传入私有文件路径，并将生成的 `bin` 放在这次 CLI 调用的 `PATH` 首位。

依次执行 `migrate`、`collect`、`events` 和上面的工作命令。真实 Raft Agent 可关联实际任务、解决 fixture 冲突、运行两个 unittest，再记录真实交付消息。该验收验证本机 CLI 与 Raft 工作流程，不能证明真实 App 推送、离线 Agent 冷启动或长时间无漏报。自动化版本包含在 `make integration` 中。
