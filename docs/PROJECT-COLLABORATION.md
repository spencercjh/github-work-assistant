# 按 fork 项目协作

同一安装可为每个 fork 配置一个工作频道。助手为每个新发现的未结束同步 PR 自动建立一个任务和固定讨论区；该 PR 的提交、CI 和协作变化继续发到这个 thread。用户可以在其中邀请其他 agent 做 review，受邀者直接回复，不需要接管同步执行者的任务。程序不调用模型，也不代替用户分派人员。

同一同步 PR 还可关联多个独立任务，分别负责同步、测试、评审和其他调查。各任务有自己的负责人和讨论区；助手保存关联、分发更新并核对提交版本。

本功能使用 External Agent 模式。所有执行者仍通过同一台机器上的本地 CLI 和数据库工作；不提供跨机器工作区调度或操作系统账号隔离。

## 配置项目频道

在已有实例的 `forks` 条目中设置 `channel`，例如：

```json
{
  "raft_mode": "external",
  "delivery_target": "#github-work",
  "decision_maker": "@owner",
  "poll_seconds": 600,
  "forks": [
    {
      "id": "engine",
      "channel": "#engine-work",
      "upstream": { "repo": "upstream/engine", "branch": "main" },
      "target": { "repo": "your-org/engine", "branch": "main" }
    },
    {
      "id": "runtime",
      "channel": "#runtime-work",
      "upstream": { "repo": "upstream/runtime", "branch": "main" },
      "target": { "repo": "your-org/runtime", "branch": "main" }
    }
  ]
}
```

这是配置片段。完整身份、存储配置和各目标仓库的可信指引仍须按[快速部署](QUICKSTART.md)和[fork 维护](FORK-MAINTENANCE.md)准备。可合并的示例见 [`examples/project-channels.json`](../examples/project-channels.json)。

`channel` 必须是顶层频道，不能是 DM 或 thread。采集身份须加入每个项目频道，配置的 `decision_maker` 须是这些频道的人类成员。用户管理参与者的频道成员资格及任务指派；`doctor` 核对采集身份和决策人的成员资格，不替用户添加成员。

显式项目频道覆盖该 fork 的 `delivery_target` 和指引中的 `task_target`。普通 PR/Issue 及未配置 `channel` 的 fork 继续使用默认投递目标。只有关闭通知自动发现、没有普通关注对象且所有 fork 均配置了频道时，才能省略默认投递目标。设置频道不会改变 GitHub 授权或轮询频率；示例中的 600 秒需由部署者明确配置。

私有项目的频道可见范围应由用户确认。项目路由不改变 `private_repositories` 的授权边界，通知仍遵守 `private_notification_details`。维护状态通知可包含登记摘要和原报告引用；私有项目关闭详细通知时，两者均不投递。

## 协调任务与工作项

每个项目保留一个协调任务，用于处理事件、管理同步队列和关联工作项。协调者必须是该任务在 Raft 中实际登记的负责人，状态为 `in_progress`。

每个同步 PR 另有自动创建的顶层任务，默认登记为该批次的 `review` 工作项，初始不指定负责人。PR 编号和项目频道确定固定入口，同一 PR 的提交更新、重复采集或进程重启不会新建替代任务。关闭旧 PR 后重建出不同编号的 PR 时，新 PR 有自己的任务；旧任务不能读取或批准替换 PR，通知也按原事件的 PR 分开投递。用户在这个 thread 中邀请的其他 agent 可以直接讨论；需要保存正式结果时，由该任务的实际负责人认领并按下述流程操作，或由协调者为其他评审者关联独立工作项。加入讨论本身不授予源代码写入、合并或独立批准权限。

项目协调任务持续存在。协调者通过 CLI 成功登记已核验的 `merged` 结果后，程序使用该执行者的正常 Raft CLI，将对应的自动 PR 任务设为 `done`；不会完成项目协调任务或额外参与者任务。后续更新不会自动重开已完成或关闭的 PR 任务。合入批次离开队列时，最后的变化仍回到已有 PR 讨论；升级时不补建已结束历史 PR 的任务。

| 工作项 | 允许的操作 | 限制 |
| --- | --- | --- |
| 协调任务 | 选择批次、重建、恢复、关联或撤销工作项 | 有有效 `sync` 工作项后，协调者不能同时执行该批的代码写入操作 |
| `sync` | 准备同步工作区、保存维护结果、运行获准的 GitHub 操作检查、运行完整验证 | 每批最多一个有效同步工作项；仍受可信仓库指引限制 |
| `test` | 读取、运行 `test-worktree`、提交自己的测试结果 | 不授予推送、合并或源工作区修改权限 |
| `review` | 读取、提交评审结果；指引已委托 `review` 时检查该 PR 的 GitHub review 操作 | 必须独立于当前及已记录的同步、协调执行者；不授予合并权限 |
| 自定义名称 | 读取、调查、提交结果 | 不授予源工作区、验证执行器或 GitHub 写权限 |

工作项类型使用小写字母开头，可含小写字母、数字和连字符，最多 40 个字符。每批最多保留 32 个工作项，包括协调关联和已撤销项。类型不可修改；撤销后不能恢复，需关联新的任务。一项参与者任务只能属于一个 PR/批次，避免复用任务导致旧版本结果或权限被带入另一 PR。

同一 PR 可有多个评审或调查任务。参与者可以并行阅读和整理报告；程序对实际工作区操作仍使用现有锁，完整验证仍使用临时合并工作区和原有的来源、映射、目标及指引校验。

## 关联参与者任务

协调任务和每个 PR 的默认评审任务由助手创建，用户指定负责人。额外参与者任务由用户或获授权的协调者创建并安排。参与者读取自己的任务讨论，使用自己的身份认领。协调者读取已确认投递的当前事件，再关联额外参与者任务：

```bash
RUN=/absolute/path/to/instance/run
# 从当前 work-context 读取这组值；不要使用历史截图中的值。
coord=(--event "$EVENT_ID" --executor "$COORDINATOR"
       --version "$EVIDENCE_VERSION" --guide "$GUIDE_VERSION"
       --task "$COORDINATOR_TASK_UUID")

"$RUN" register-work-item "${coord[@]}" \
  --participant-task "$SYNC_TASK_UUID" --kind sync
"$RUN" register-work-item "${coord[@]}" \
  --participant-task "$TEST_TASK_UUID" --kind test
"$RUN" register-work-item "${coord[@]}" \
  --participant-task "$REVIEW_TASK_UUID" --kind review
```

关联会核对任务实际存在于项目频道，但不会认领、改派或改变任务状态。任务未开始或负责人不匹配时，参与者不能执行。旧负责人在人工交接后立即失去执行资格；撤销工作项也会停止该任务的执行资格。

协调者可在关联命令加 `--batch <batch_id>`，为队列中另一批关联任务。默认使用当前选中的批次。批次重建保留 `batch_id`，手动关联的批次工作项继续适用；自动 PR 任务还绑定具体 PR 编号，不能随批次转到另一 PR。重建产生的新轮次不会继承旧测试或评审结果的有效状态。

## 参与者读取与报告

仓库可安装 [上游同步 skill](../skills/upstream-sync/SKILL.md)，将下面的命令串成刷新、投递、关联和保存上下文的入口。部署者提供本机实例 `run` 路径，执行者仍使用自己的实时任务归属。仓库文件只保存项目绑定，不保存凭据、运行时事件版本或私有上下文。

在目标仓库的独立工作区生成文件，然后按该仓库规则审阅并提交：

```bash
python3 skills/upstream-sync/scripts/sync_work.py install \
  --destination /path/to/fork/.agents/skills/upstream-sync \
  --fork engine --target your-org/engine --target-branch main \
  --upstream upstream/engine --upstream-branch main --mode upstream-only
```

已有目录不会被覆盖。更新时先生成到另一目录，对比变更，再按仓库流程替换。skill 自身是下游维护文件；原样同步模式不要将其修改放入上游同步 head。安装不需要重启服务，也不替代下面的程序检查。

参与者用自己的任务 UUID 读取上下文。程序据此找到对应批次，无须切换同步工作区：

```bash
"$RUN" work-context --event "$EVENT_ID" --task "$MY_TASK_UUID"
"$RUN" verify-claim --event "$EVENT_ID" --task "$MY_TASK_UUID" --executor "$MY_HANDLE"

work=(--event "$EVENT_ID" --executor "$MY_HANDLE"
      --version "$EVIDENCE_VERSION" --guide "$GUIDE_VERSION"
      --task "$MY_TASK_UUID")
"$RUN" link-task "${work[@]}"
```

`work-context.collaboration.items` 包含各工作项的任务、自己的证据关联和最近结果；`pr_discussion=true` 标识自动创建的 PR 固定任务，`pr` 是它绑定的 PR 编号。`work-context.task` 随 `--task` 返回该任务的关联，不覆盖其他参与者。当前事件尚未送达、证据过期或任务归属变化时，停止操作，刷新上下文并处理具体错误。

测试执行者按原可信配置运行 `test-worktree`。实际源工作区操作和完整验证要求该批次是协调者选中的批次；评审、读取、关联和报告可处理其他排队 PR，不改变当前选择。队列选择不会自动中断正在运行的验证。

其他排队 PR 的非失败 CI 进展、普通评论和评审更新不会单独使当前测试结果失效。来源、目标、依赖关系、冲突、前批失败或关闭、新的 `CHANGES_REQUESTED`、当前 PR 的反馈和执行权限变化仍会阻止保存旧证明。完整反馈继续保存在上下文中，保存测试证明不授权使用旧事件写入；写入前仍须读取并处理最新反馈。

报告先通过 `raft message send` 发布到自己任务的 thread，再使用其完整消息 UUID：

```bash
"$RUN" record-contribution "${work[@]}" \
  --report "$REPORT_UUID" --outcome approved \
  --head "$REVIEWED_HEAD" --base "$REVIEWED_TARGET" \
  --summary-file /absolute/path/to/review-summary.md
```

支持的结果为 `investigated`、`waiting`、`passed`、`failed`、`approved`、`changes-requested`。`passed`/`failed` 仅供 `test` 工作项使用，`approved`/`changes-requested` 仅供 `review` 使用。助手核对报告确实由当前执行者发在自己的任务讨论区，结果与当时的轮次、head、目标提交、文件树、映射和指引绑定。

`passed` 还须绑定该任务、该执行者实际完成的验证尝试。`approved` 需要当前完整验证证明，且评审者不能是本批同步或协调的执行者。授予源操作前会保存当前执行者身份；交接后复用旧关联，也不能漏过这项记录。交接、撤销或删除同步任务不会抹除已记录的身份；撤销后的评审核对使用历史记录，不要求该旧任务仍然存在。

`result.current=true` 表示结果仍匹配最新采集状态和保存的完整证明，不是永久授权，也不表示读取了尚未采集的 GitHub 变化或未提交的本地修改。目标、提交、映射、指引或验证尝试变化后，应按最新上下文判断是否需要重新验证或审查。该记录不替代 GitHub/Stamp 审批、仓库保护规则或合并检查。发布 review 仍需单独的 `review` 委托及 `check-github-action --action review`；其他写操作不会因加入评审任务而开放。

协调者继续使用 `record-work-result` 记录维护结果；若已委托 `sync` 工作项，由同步执行者记录该批维护结果。参与者的调查结果不会推进同步完成位置或将失败验证变成通过。

## 等待原因与合入收尾

维护结果的摘要应写清正在等待的条件、已发出问题或报告的位置，以及负责续接的人。程序将 `waiting`、`needs-decision`、`changes-ready` 和 `merged` 的摘要与引用送到对应 PR 的固定讨论区，重复使用同一报告不会重复发送同一状态，也不会再次提及问题中的决策人。

| 实际情况 | 登记方式 | 队列影响 |
| --- | --- | --- |
| 检查尚未结束，或外部条件未满足 | `waiting` | 本报告不能使后续批次通过准备检查；设置持久续接提醒 |
| 验证缺失、检查失败或范围尚需用户决定 | `needs-decision` | 阻止后续批次继续；报告须满足原有的决策人通知要求 |
| 验证和检查满足要求，仅等待人工审阅或合入 | `changes-ready`，或 `needs-decision --review-only` | 仍须通过全部前序批次、来源连续性和容量检查，才可准备后批 |

没有远端 CI 记录不等于检查通过；通知也不替代项目指引或豁免决定。`fork.work_status` 保存最近登记的报告，表示登记时状态，不授予当前执行权限。执行前仍须读取当前提交、目标、反馈和指引。

合入登记与待收尾记录在同一事务中保存。若 `record-work-result` 返回 `recorded=true` 和 `task_completion_pending`，合入结果已经保存，不要重新验证或再次合并。读取原讨论并解决状态写入阻塞后，由当前协调者重试：

```bash
"$RUN" finish-work "${coord[@]}"
```

独立 `sync` 工作项或 HTTP 入口保存合入结果后，也由协调者执行该命令。它只处理已有核验合入记录对应的自动 PR 任务，回读确认 `done` 后保存收尾回执；批次归档不丢失该记录，无关队列事件也不会阻止收尾。任务已撤销、由用户负责、暂停或路由发生变化时，保留错误供核对。完成过的收尾记录不会覆盖之后的人工重新开启。采集身份不冒用执行者更新任务状态。

## 更新投递与恢复

每个新发现的未结束 PR 先建立固定任务。GitHub 变化追加到项目协调讨论，并发送到受影响 PR 的固定 thread 和额外参与者 thread。关联、撤销和报告也会生成明确标记的本地更新；报告作者不会因自己的报告再次收到参与者通知。完整报告留在原讨论，维护状态通知带登记摘要和结果引用。

发送前固定接收任务列表。每个接收者保存独立请求和回执；部分失败或进程重启后复用原内容和幂等键。只有全部仍有效的接收者确认后，事件才记为 `accepted`。已经完成的任务收到更新不会自动重开；冻结后撤销的任务停止后续投递；已发出的通知可能仍可见，但执行检查按最新撤销状态拒绝操作。

PR 任务创建也在发送前保存完整请求，回执丢失后复用原请求和幂等键。PR 此时已关闭或批次已归档，仍须完成原创建请求的确认；回执与任务关联一起保存后才能接受事件。请求结果不明且超过 23 小时的安全重试窗口时，停止重发并核对原消息。任务被删除而使投递阻塞时，协调者可用旧的已送达事件作为协调回执，读取并传入当前证据和指引版本，显式撤销该工作项后重试投递；自动 PR 任务被撤销后也不会创建替代任务。这个例外只用于关联管理，不允许旧事件执行代码操作。采集身份失去频道权限或保存的发送身份不匹配时，保留错误，先恢复或核对原投递，不另建替代任务绕过问题。

## 已有实例切换

切换前，先让执行者停止新写入，等待当前验证保存结果，并处理完旧目标的待投递事件。保存配置、数据库和原二进制备份。

由部署者升级程序并修改 `forks[].channel`，核对新频道的成员，更新实例的 `EXECUTOR.md`。运行 `check-config` 和 `doctor` 后，再通过一个明确的 `request-work` 请求触发新目标的投递，并核对实际收到的协调任务。仅替换程序不会改写配置、自动移动消息或恢复被用户暂停的工作。

同一频道启用项目协作时会复用原协调任务；若已有多个历史任务，需由部署者用 `adopt-discussion` 明确选择。频道改变后，新频道建立自己的协调关联，旧讨论和报告保留。旧任务回执不再授予新频道的执行权，参与者须在新频道重新关联。频道配置属于指引版本，旧证明必须按新版本要求重新验证后才能用于写入。未完成的旧投递不得直接改道，应先按原配置完成或核对；移除 `channel` 也不会把参与者权限自动迁入默认任务。

协作状态和引用保存在现有数据库中，不需要新的数据表。报告正文沿用历史保留规则；任务关联、版本及发送幂等记录与数据库一起备份。部署者应在实际项目频道验证一次接收和分工流程；本地合成回归不代替真实账号验收。
