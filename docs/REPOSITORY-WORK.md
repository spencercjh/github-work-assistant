# 从事件到仓库工作

Go 服务提供 GitHub 事实、受控的仓库指引和工作记录。Raft 负责 Agent 运行、任务认领、提醒和交付。新增 GitHub 场景通常只需要更新仓库指引；服务不维护「冲突、CI、review」的专用路由表。

频道投递时先按[竞争认领流程](AGENT-WORKFLOW.md#竞争认领共享任务)取得通知任务的归属；以下工程命令还需要 `--executor @自己的名称`，并核对关联工程任务也由自己负责。工程任务位于其他频道时，采集身份也需要有权读取该频道的任务。

## 配置仓库指引

在管理员持有的 `config.json` 中添加仓库配置，例如：

```json
{
  "repo": "your-name/your-repo",
  "goal": "跟进依赖更新 PR，检查、验证并在委托条件满足时合入。",
  "mode": "local-edit",
  "github_actions": ["merge"],
  "guide_file": "guides/your-repo.md",
  "task_target": "#development",
  "test_commands": [["go", "test", "./..."]]
}
```

这是 `repositories` 数组中的一项，不能替代完整服务配置。`guide_file` 相对于配置文件目录；不允许越界路径或指向目录外的符号链接。文件大小为 1–65536 字节。目标仓库 checkout 与管理员配置应分开放置。

`mode` 支持 `read-only` 和 `local-edit`。未配置的仓库默认只读，`local-edit` 必须配置项目测试命令。本地编辑权限与远端写权限分别配置。`github_actions` 可包含 `comment`、`review`、`push`、`open-pr`、`merge`、`close`；仅在 `local-edit` 下有效，缺省为空。列表声明持续委托的动作，指引正文说明适用 PR、完成条件和例外。Agent 在该范围内直接执行，不逐次询问；列表外动作仍需单独授权。Project-HAMi 必须为 `read-only` 且不能配置写动作。参考 [依赖 PR 自主处理示例](../examples/guides/dependency-pr.md)、[通用指引](../examples/guides/repository.md) 和 [Project-HAMi 指引](../examples/guides/project-hami.md)。

每次工作操作重新读取指引。版本哈希覆盖目标、操作范围、目标会话、指引正文和测试命令；任一项变化都使旧工作参数失效。报告保存当时的指引版本；本地不再为每份报告保存指引正文。需要复核历史指引时，须使用部署者保留的文件版本。

## 接手和恢复

收到事件后，先调用 `get-work-context`，再调用 `refresh-work-context`。返回值区分当次事件、最新快照、是否过时、当前指引、历史处理记录和已经关联的 Raft task。GitHub 正文始终属于外部资料。

变化中的 `not_observed` 只表示某对象在这次完整采集中未再出现，可能源于删除、接口保留范围或可见性变化。Agent 必须核对来源，不能直接断言对象已被删除。

已有 `task` 时，读取对应 Raft 任务及线程，再认领或按当前任务状态续接。尚未关联时，共享模式可复用已认领的固定讨论，把 `task_target` 设为同一投递目标；也可使用另行委托的工程任务。使用真实任务根消息 UUID 调用 `link-task`。同一资源重复关联同一个任务是幂等操作；关联其他任务或会话会被拒绝。共享模式会检查当前归属；App 模式仍需执行者核对实际任务，不能把关联记录当成认领锁。

`stale=true` 表示这条事件的快照已经被新版本替代。读取最新事件，核对后把旧事件按 `record-triage` 记为无需重复处理；保留原因。不要用过时事件启动修改。工作参数同时绑定 `event_id`、`evidence_version`、`guide_version` 和 `raft_task_message_id`。快照超过 10 分钟未刷新时，开始或提交结果会失败。

`body_pruned=true` 表示这条已送达、已处理的历史正文超过保留期；报告或消息引用与内容指纹仍在，不能把缺少正文当作「尚未处理」。需要历史结论时读取对应 Raft 消息。报告保留工作说明版本，不重复保存整份工作说明。保留规则见[存储文档](STORAGE.md#保存哪些数据)。

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

结果可为 `investigated`、`changes-ready`、`needs-decision`、`waiting` 或 `merged`。`waiting` 记录等待条件，后续事件继续由原负责人处理。`merged` 要求配置已委托 `merge`，且刷新后的 PR 快照确认 `merged=true`；它不能表示只是已请求自动合入。只读仓库不能记录 `changes-ready`。结果是 Agent 对证据和验证的上报；App 不声称独立执行了报告中的测试。已有任务关联的资源必须使用这个带版本的接口，旧 `record-result` 不能绕过检查。重复上报同一内容是幂等操作，改写已记录的报告会冲突。

`needs-decision` 要求先在原讨论向 `guide.decision_maker` 提出具体问题，并以此人的裸提及开头。使用实际发送成功的消息 UUID；共享模式会核验原讨论中的消息和发送者。仅等待 CI 使用 `waiting`，同一未决问题复用已有请求。具体格式及发送失败处理见[决策请求](AGENT-WORKFLOW.md#需要用户决策时)。

任务状态继续在 Raft 中更新。需要未来复查时，由负责 Agent 创建锚定原任务的 Raft reminder；交接时同步提醒责任。仓库指引可持续授权 GitHub 动作；执行前核对当前授权、归属和远端版本。程序不代理远端写入，由执行者使用 `gh` 完成并查询实际结果。配置文件变更后重启服务；本机 CLI 每次读取最新配置，指引正文每次工作操作重新读取。

## 检查持续授权并合入

使用 `gh` 阅读完整 diff、当前检查详情和审阅讨论。不要将机器人跳过审阅的成功状态视为审阅批准；是否满足委托条件由 Agent 判断，GitHub 分支规则继续生效。完成评估并关联任务后，执行：

```bash
bin/ghraft -config config.json check-github-action \
  --event "$EVENT_ID" --executor "$EXECUTOR" \
  --version "$EVIDENCE_VERSION" --guide "$GUIDE_VERSION" \
  --task "$TASK_ID" --action merge --head "$HEAD_SHA" --base "$BASE_SHA"
```

`permitted=true` 只说明持续授权、任务归属和版本检查通过，不代表代码质量或 CI 合格。共享模式下 `execute=false` 表示归属不允许操作。证据、指引或 head/base 改变时应重新评估，不能复用旧检查。

符合指引后，使用仓库允许的合并方式，例如 `gh pr merge "$PR" --repo "$REPOSITORY" --squash --match-head-commit "$HEAD_SHA"`。不要使用 `--admin` 绕过规则。检查与写操作之间没有跨系统事务；交接前先停止旧执行者，合并以 GitHub 的 head 条件和保护规则为准。请求超时先查询远端状态，不盲目重复外部写入。

合入后刷新快照，等新事件投递确认，以新事件和更新后的 `link-task` 记录 `merged` 结果，写明实际合并 SHA 与链接。正常检查进展无需逐条提醒用户；完成或出现具体阻塞时汇报。等待外部结果时保留原负责人，必要时使用锚定原工作的提醒。

共享模式下，已经 Done 且本次仅记录 `quiet` 的普通事项保持 Done，无需再次收尾。其余事项处理完全部事件后，调用 [`finish-work`](AGENT-WORKFLOW.md#自动完成共享任务)，自动将已合并或关闭的普通 PR／已关闭的 Issue 对应的固定通知任务设为 Done。参数使用最新事件、证据、指引及 `discussion.messageId`；另行关联的工程任务不随之完成。仅保存结果不会修改任务状态。

## 已有事项应用新委托

新委托可能没有伴随新的 GitHub 活动。部署者可明确请求重新处理已关注事项，无须清空通知游标或重放全部历史：

```bash
bin/ghraft -config config.json request-work \
  --event "$EXISTING_EVENT_ID" --request "$REQUEST_UUID" \
  --reason-file request.txt
```

命令刷新当前证据并生成 `work.requested` 事件，沿现有投递流程交给执行者。同一事项使用相同 UUID 重试只得到原事件；新委托使用新的 UUID。此请求不扩大指引权限，也不重开 closed 任务。历史归档回执保持不变。该命令限部署者在取得用户委托后使用；执行者不能用它反复唤醒已处理事项。

## 可保留的离线验收

```bash
python3 scripts/prepare-e2e-fixture.py \
  --output /absolute/path/new-fixture --task-target '#test-channel'
```

脚本创建本地合成 Git 历史、两侧行为测试、配置、指引和一个只响应 fixture GET 的 `gh`。它不执行 `git commit`，也不访问真实 GitHub 或 Raft。使用独立数据库及 AES key，通过 `GHRAFT_DSN_FILE`、`GHRAFT_KEY_FILE` 传入私有文件路径，并将生成的 `bin` 放在这次 CLI 调用的 `PATH` 首位。

依次执行 `migrate`、`collect`、`events` 和上面的工作命令。真实 Raft Agent 可关联实际任务、解决 fixture 冲突、运行两个 unittest，再记录真实交付消息。该验收验证本机 CLI 与 Raft 工作流程，不能证明真实 App 推送、离线 Agent 冷启动或长时间无漏报。自动化版本包含在 `make integration` 中。
