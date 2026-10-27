# 持续维护有定制改动的 fork

这项功能跟踪指定上游分支，把更新整合进保留定制能力的 fork。程序负责发现变化、保存轮次、关联同步 PR 和核对证据；执行者负责理解改动、适配代码、运行测试，并按可信仓库指引完成获准的 GitHub 操作。

每条规则沿用一项工作和同一负责人。同一时刻只有一轮同步，每轮固定上游提交；期间出现的新上游提交留到下一轮处理。合入并完成远端核验后，才推进成功位置。

## 配置维护规则

先按[快速部署](QUICKSTART.md)安装助手。使用同一个 GitHub 账号和操作系统账号；准备目标项目需要的编译、测试工具。首版面向 GitHub 同一 fork 网络中的仓库，使用保留祖先关系的合并方式。目标仓库需要允许 merge commit；squash、rebase、强制推送和直接推送目标分支不属于本流程。

在实例的 `config.json` 中增加 `forks`，并为目标仓库增加明确的 `repositories` 指引。以下片段合并到已有配置，不能替代完整安装配置：

```json
{
  "forks": [
    {
      "id": "custom-engine",
      "upstream": { "repo": "upstream/engine", "branch": "main" },
      "target": { "repo": "your-org/engine", "branch": "main" }
    }
  ],
  "repositories": [
    {
      "repo": "your-org/engine",
      "goal": "持续吸收上游更新，保留并验证部署适配和自有行为。",
      "mode": "local-edit",
      "guide_file": "guides/engine.md",
      "task_target": "#github-work",
      "test_commands": [["make", "test"], ["make", "test-custom"]],
      "github_actions": ["push", "open-pr"]
    }
  ]
}
```

将 `examples/guides/fork.md` 复制到实例的 `guides/engine.md`，填写实际定制行为、验收条件和发布限制。`test_commands` 是参数数组，直接执行，不经过 shell 展开。修改配置后先运行 `"$INSTANCE/run" check-config`，通过后按部署方式重启采集服务。

| 配置 | 含义与限制 |
| --- | --- |
| `id` | 稳定的维护规则名称，小写字母、数字和连字符，最长 48 字符。持久化标识为 `fork:<id>`。已有规则的仓库或分支关系不能原地更换，需使用新名称。 |
| `upstream` | 只读来源。仓库名按 GitHub 大小写规则匹配，分支名保留大小写。 |
| `target` | fork 仓库及目标分支。一个目标分支只能有一条规则。 |
| `guide_file` | 由部署者维护，位于配置目录内；不能来自正在整合的 PR。说明定制能力的目的、测试方法和允许的操作。 |
| `test_commands` | 必填。覆盖上游行为和自有行为；只有编译成功不足以证明定制能力仍然成立。 |
| `github_actions` | 默认没有远端写权限。示例允许推送同步分支和创建 PR，由人合入。需要自动合入时，再明确加入 `merge` 并写清合入条件。 |

仓库指引仍按仓库生效，应写明授权只适用于哪些维护工作，不能把它理解为该仓库所有 PR 均获准操作。Project-HAMi 社区目标仍仅允许调查，因此不能配置为可修改的维护目标。

分支采集独立于 Notifications。可以只配置 `forks` 并关闭 `discover_notifications`、清空 `watches`。如果上游和目标已经没有待整合提交，首次采集只建立背景状态，不产生工作。

任一来源为私有仓库时，必须在 `private_repositories` 中允许访问。详细通知继续受 `private_notification_details` 控制。曾读取过的私有来源也受当前许可约束：移除许可后，不能通过后来变为公开的仓库读取旧私有上下文。

## 理解一次同步

```mermaid
flowchart TD
  A[读取上游和目标分支] --> B{存在待整合提交？}
  B -->|是，且没有未完成轮次| C[固定本轮提交，建立同步分支名]
  C --> D[原负责人准备工作区、理解并适配改动]
  D --> E[提交后运行上游及定制功能测试]
  E -->|失败| D
  E -->|通过| F[核验授权、提交和分支范围]
  F --> G[推送同步分支，创建或更新同一个 PR]
  G --> H[响应评审及 CI；按授权自动合入或由人合入]
  H --> I[核验实际合并提交的树与祖先关系]
  I --> J[记录成功，再处理期间出现的新上游提交]
```

程序根据本轮稳定分支名发现已经创建的 PR。因此创建请求超时后，先刷新上下文；若 PR 已存在，继续使用它，不重复创建。PR 的评论、评审和检查从普通采集器读取，但归回维护工作，不再形成独立工作。

`work-context` 中 `fork` 是可恢复的进度，`snapshot.objects.fork` 是本次采集到的远端事实：

| 字段 | 用途 |
| --- | --- |
| `round`、`upstream_sha`、`initial_target_sha` | 本轮身份、固定上游和初始目标提交。 |
| `observed_upstream_sha` | 最近看到的上游位置，可能比本轮更新；不会打断当前适配。 |
| `sync_branch`、`pr` | 本轮工作分支和关联 PR；尚未创建时 `pr` 为 0。 |
| `validated_head`、`validated_base`、`validated_guide` | 已通过测试的代码、测试时目标提交及指引版本。 |
| `last_success_upstream_sha` | 已核验并记录成功的上游位置；发现、通知、测试通过都不会推进它。 |
| `next_attempt`、`retries` | 恢复处理的时间和已触发次数，时间为 Unix 秒；0 表示当前不安排自动唤醒。 |
| `current_event_id` | 目前证据对应的事件。旧通知过期时，读取此事件并等待投递确认。 |

## 准备、适配和测试

先遵循[执行流程](AGENT-WORKFLOW.md)取得当前归属。刷新证据，读取完整指引和已有结果。下面的变量分别来自当前上下文与实际工作记录，不可自行编造；刷新后若 `stale=true`，使用 `current_event_id` 对应的新事件，等它送达后重新核验并关联。

```bash
"$INSTANCE/run" refresh-work-context --event "$EVENT_ID"

# 更新 EVENT_ID、VERSION、GUIDE 后，建立本轮通用参数。
work=(--event "$EVENT_ID" --executor "$EXECUTOR"
      --version "$VERSION" --guide "$GUIDE" --task "$TASK_ID")
"$INSTANCE/run" link-task "${work[@]}"
```

使用 `gh repo clone` 准备目标仓库的本地克隆，确认 `origin` 指向目标仓库。获取本轮固定上游与当前目标的提交对象；检查实际 SHA，不能用正在移动的分支名替代本轮输入。程序的工作区命令只操作本地 Git，不负责下载代码。

```bash
"$INSTANCE/run" prepare-worktree "${work[@]}" --repo "$LOCAL_CLONE"
```

命令返回独立工作区路径和冲突文件。执行者分析上游改动对定制功能的影响，处理文本冲突和行为适配，按项目的提交身份、签名与测试约定提交代码。程序不自动提交。

再次运行 `prepare-worktree`，将期间前进的目标分支也合并进来。每次未提交的 merge 都必须先解决并提交，再继续准备。未解决的冲突、未提交的改动、缺失的目标历史都会阻止验证；已有轮次始终复用原工作区，不丢弃适配。

```bash
"$INSTANCE/run" verify-worktree "${work[@]}"
"$INSTANCE/run" test-worktree "${work[@]}"
```

`test-worktree` 执行可信指引的全部命令，要求开始和结束时工作区干净、提交相同。成功返回 `passed=true`，并保存提交、目标、指引版本及测试日志摘要；任一测试失败都会清除上一份可用测试证明。`verify-worktree` 只检查工作区与历史，不证明测试已执行。

测试总时限为 15 分钟，输出上限为 16 MiB。测试子进程使用临时 HOME 和有限环境变量，不继承 GitHub token 等环境凭据；它仍可访问同一系统账号可读的文件，不是操作系统沙箱。需要凭据或专门环境的测试，应由部署者先建立受控测试入口，不能把密钥写进参数或指引。

## 推送、评审和合入

任何远端写入前，先检查授权及当前事实。`HEAD_SHA` 是刚通过测试的提交，`TARGET_SHA` 来自最新目标分支，`SYNC_BRANCH` 来自本轮进度：

```bash
"$INSTANCE/run" check-github-action "${work[@]}" \
  --action push --head "$HEAD_SHA" --base "$TARGET_SHA" --ref "$SYNC_BRANCH"
```

只有 `permitted=true` 才通过检查。推送必须保留远端工作分支的历史，不能使用强制推送；若远端工作分支有其他提交，先获取并整合，再测试。推送后刷新并关联新证据，再以 `--action open-pr` 和相同分支检查创建 PR；返回的 `repository`、`number`、`branch`、`target_branch` 是操作范围。已有 PR 时继续使用它。

程序不会代执行者写入 GitHub。执行者仍需遵守仓库约定，使用 `gh` 创建 PR、读取评审与 CI，解释定制能力的保留方式。代码、目标分支或可信指引变化后，必须重新整合和测试。保留失败测试的原因与必要的人类决策，不能为追求通过而移除定制功能。

自动合入还需配置 `merge` 权限，并满足指引中的审阅和 CI 条件：

```bash
"$INSTANCE/run" check-github-action "${work[@]}" \
  --action merge --head "$HEAD_SHA" --base "$TARGET_SHA" --ref "$TARGET_BRANCH"
```

该检查核对本地已测试提交、远端 PR head/base、目标分支和当前归属；它不判定审阅质量，也不锁定 GitHub 分支。执行者应使用 `gh pr merge --merge --match-head-commit "$HEAD_SHA"`，不绕过保护规则。检查和远端写入之间仍可能有并发更新，必须核验实际结果；合并队列中的等待状态不代表已合入。

合入后刷新上下文，使用新事件与版本重新 `link-task`，在原讨论中报告，再记录：

```bash
"$INSTANCE/run" record-work-result "${work[@]}" \
  --report "$REPORT_ID" --outcome merged --summary-file result.txt
```

成功需要实际 PR 已合入、PR head 等于通过测试的提交、实际 merge commit 保留该提交及本轮上游历史，并且 merge commit 的文件树与测试树一致。merge commit 必须仍是目标分支祖先。目标分支随后前进不妨碍记录这一轮成功；这不表示后来新增的提交也通过了原测试。实际合并树不同则不能记录成功，应交由人工判断并补充验证。

仅授权创建 PR 时，由人合入后也可记录经过核验的 `merged` 事实；这不会赋予执行者合入权限。

处理完全部事件后按[任务收尾流程](AGENT-WORKFLOW.md#自动完成共享任务)调用 `finish-work`。同步 PR 合并只完成当前轮次，持续维护任务保持 `in_review`；后续上游变化继续使用原任务。

## 等待与恢复

| 情况 | 行为 |
| --- | --- |
| 等待 CI 或评审 | 保存 `waiting` 和具体继续条件；15 分钟后可唤醒原负责人，也会响应新远端事件。 |
| 代码已验证、等待后续操作 | 保存 `changes-ready`，1 小时后可继续检查。 |
| 执行中断、没有结果 | 新轮次首次 4 小时后恢复，后续按重试次数退避；一轮累计触发 5 次后进入 `needs-decision`，停止定时唤醒。 |
| 需要业务取舍或测试环境不足 | 保存 `needs-decision` 和具体问题，等待人明确下一步；新远端变化仍可被记录。 |
| PR 关闭而未合入 | 进入 `needs-decision`。决定重新尝试后运行 `restart-sync`，下一次采集建立新轮次，不继承旧验证或成功位置。 |
| 目标分支前进 | 旧测试证据不能用于写入；刷新、准备工作区、提交、重新测试。 |
| 服务退出后重启 | 保留数据库与工作区。恢复原轮次、原负责人和已创建 PR，不另建工作。 |
| 用户停止维护 | 关闭原工作会停止执行；从配置移除规则并重启会停止该规则采集。恢复需按原记录和许可核对，不删除旧进度来绕过限制。 |

`restart-sync` 需要当前事件、归属和版本参数，只适用于已关闭且未合入的 PR。它不会关闭 PR、删除工作区、强制推送或把放弃的轮次算作成功。

`"$INSTANCE/run" status` 的 `forks` 包含保留的进度，`fork_errors` 包含最近采集错误；删除配置不会删除历史进度，因此这些字段也可能包含已停用规则。查询失败时不推进成功位置。工作区和测试日志保留在 `workspace_root`，需在不再需要恢复后由部署者清理；现有事件正文保留策略不删除源码工作区。

## 实现与验证边界

`WorkDefinition` 统一解析普通事项和维护规则；`ForkSource` 增加分支事实采集，纯函数规划下一轮，工作区及结果核验各有对应实现。两类工作复用事务事件、可靠投递、固定负责人、可信指引和报告关联。轮次进度和 PR 关联保存在现有关系存储中，与快照和待发事件一起提交，避免进度与通知错位。

自动化测试使用真实本地 Git 历史与模拟远端：覆盖定制功能回归失败、目标移动后重测、人工合入、合入后分支前进、错误合并树、创建 PR 响应丢失、关闭后重试、重启、并发采集、负责人交接和私有访问撤回。运行 `go test -race ./...`。这不等同于真实 fork 的部署验收；实际接入时仍需选择项目、配置定制行为与测试，并核对一轮真实 PR 的结果。
