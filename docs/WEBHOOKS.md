# 通过 GitHub Webhook 接收更新

`github_source` 在轮询和 Webhook 之间二选一，默认值为 `polling`。Webhook 模式适合能为关注仓库配置 Webhook，并提供 HTTPS 接收地址的部署。

| 配置 | 后台行为 |
| --- | --- |
| `"github_source": "polling"`，或省略 | 按 `poll_seconds` 采集；沿用 Notifications 和已结束事项停止轮询的规则；Webhook 接口不可用。 |
| `"github_source": "webhook"` | 按已接收的事件刷新配置中的关注对象或 fork；不定时扫描 GitHub，不轮询 Notifications，也不自动回退到轮询。 |

Webhook 只是刷新信号。助手收到事件后，仍通过现有 GitHub 身份读取最新状态，再生成变化和通知。签名正确不代表获得新的仓库访问或写入权限。未配置的仓库、Issue、PR 和 fork 不会因收到事件而自动加入维护范围。

本地测试超时恢复、待发通知重试和任务收尾继续运行。投递前权限校验、收尾校验及主动执行的 `collect`、`refresh-work-context` 等操作仍可能实时查询 GitHub。`poll_seconds` 在 Webhook 模式下用于待发通知的重试间隔，不启用周期采集。

## 部署前提

- 服务主机能访问 GitHub 和 Raft，且现有 GitHub 身份能读取关注仓库。
- 部署者能够管理相关仓库的 Webhook，或通过有对应范围的组织／GitHub App Webhook 转发事件。
- 提供有效证书的 HTTPS 地址，将 `/webhooks/github` 转发到助手监听地址。默认只监听 `127.0.0.1:4185`；程序不自动申请证书或创建公网入口。
- 对私有仓库，仍需配置 `private_repositories`；仅设置 Webhook 不会扩大访问范围。

持续 fork 维护需要接收目标仓库和上游分支的事件。没有上游 Webhook 管理权限时，目标仓库的 Webhook 无法通知上游的新提交；该模式下需要主动刷新才能发现这些变化。若必须自动跟踪这类上游，应选择 `polling`。

## 新建实例

先按[快速部署](QUICKSTART.md)准备 GitHub 登录和发送凭据，再增加事件来源参数与固定关注对象：

```bash
bin/ghraft init --dir state/assistant \
  --raft-token-file /private/external-token \
  --recipient @reviewer --decision-maker @owner \
  --github-source webhook --watch 'owner/repo#123'
state/assistant/run migrate
state/assistant/run doctor
```

`init` 为 Webhook 生成独立的随机密钥，保存到实例的 `private/github-webhook-secret`，权限为 `0600`。GitHub Webhook 的 Secret 字段使用该文件内容，去掉末尾换行。不要把密钥放入 URL、日志或版本库。这个密钥不同于 GitHub 访问令牌、Raft 凭据和数据库加密密钥。

当前初始化命令要求至少一个 `--watch`。只维护 fork 的实例，可在启动前将生成的 `watches` 清空，添加有效的 `forks` 配置，并运行 `check-config`；配置中始终需要至少一个固定关注对象或 fork。

## 为已有实例切换模式

切换前停止服务并备份数据库、实例配置和私有文件。使用新二进制执行 `migrate`，创建 Webhook 收件表；不要跳过迁移直接启动新版本。

在现有 `config.json` 中合并以下字段，保留原有身份、数据库和工作范围：

```json
{
  "github_source": "webhook",
  "discover_notifications": false,
  "listen": "127.0.0.1:4185",
  "watches": [{"repo": "owner/repo", "number": 123}]
}
```

Webhook 模式禁止 `discover_notifications=true`。此前通过 Notifications 发现的对象不会自动成为 Webhook 关注对象；需要继续跟踪的对象应显式加入 `watches`，同步 PR 由对应的 `forks` 规则处理。

若密钥文件尚不存在，可在实例目录执行以下命令。排他创建可防止覆盖已有密钥：

```bash
python3 - <<'PY'
import os, secrets
fd = os.open('private/github-webhook-secret', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as f:
    f.write(secrets.token_hex(32) + '\n')
PY
```

程序要求密钥至少 32 字节，文件必须为私有普通文件，不能是符号链接。直接调用二进制时可用 `GHRAFT_GITHUB_WEBHOOK_SECRET_FILE` 指定其他文件；生成的 `run` 入口固定使用本实例文件。

执行 `run check-config` 和 `run doctor` 后启动服务。`doctor` 只检查本地密钥及既有连接，不能证明 GitHub 已配置 Webhook 或公网转发可达。

## 配置 GitHub 投递

在需要接收事件的仓库中，进入 Settings → Webhooks → Add webhook：

| 设置 | 值 |
| --- | --- |
| Payload URL | `https://assistant.example.com/webhooks/github`，替换为实际 HTTPS 地址 |
| Content type | `application/json` |
| Secret | 本实例 `private/github-webhook-secret` 的内容 |
| SSL verification | 保持启用 |
| Active | 启用 |

反向代理应保留原始请求正文以及 `X-Hub-Signature-256`、`X-GitHub-Delivery`、`X-GitHub-Event`，不改写或重新编码 JSON。助手最多读取 25 MiB；超过限制返回 `413`。只需转发此接收路径，不要把本地管理接口一并公开。

按维护范围选择以下事件，GitHub 界面名称可能与事件标识略有不同：

| 需要观察的变化 | 事件标识 |
| --- | --- |
| Issue 状态、说明及评论 | `issues`、`issue_comment` |
| PR 状态、提交及评审 | `pull_request`、`pull_request_review`、`pull_request_review_comment`、`pull_request_review_thread`，以及 `issue_comment` |
| 检查与工作流结果 | `check_run`、`check_suite`、`status`、`workflow_run` |
| 上游、目标及同步分支 | `push`、`create`、`delete` |
| 仓库属性变化 | `repository` |

分支事件只匹配已配置的上游、目标、当前同步分支或已观察 PR 的相关分支。CI 事件优先按 PR 编号匹配；如果只带提交 SHA、没有关联 PR，则刷新该仓库内已配置的对象，避免新 head 的通知早于快照更新而被丢弃。Webhook 类型和权限决定能够订阅哪些事件，参见 [GitHub 事件与载荷说明](https://docs.github.com/en/webhooks/webhook-events-and-payloads)。

保存后，GitHub 的 `ping` 应返回 `200`。启用 Webhook 后主动执行一次 `run collect`，建立现有对象的快照；`ping` 不执行初始采集。随后检查一次真实事件：接收返回 `202`，后台刷新完成后 `status` 中的 `webhooks.pending` 应下降，相关对象的快照和处理记录应与最新 GitHub 状态一致。

## 接收、重试与去重

接收端先校验原始正文的 HMAC-SHA256 签名，再解析事件。它只保存投递 ID、正文指纹、事件类型、待刷新对象和重试状态，不保存原始 Webhook 正文。`202` 表示接收记录已持久保存，或该投递已接收；不表示 GitHub 刷新、通知投递或执行工作已经成功。

后台合并同批到期记录中指向同一对象的刷新请求。服务重启后继续读取持久队列；刷新失败的对象保留，已成功的对象不随同一投递重复刷新。普通失败从 30 秒开始指数退避，上限 1 小时。识别到 GitHub 限流时，还会保存整个 Webhook 刷新器的冷却时间，从 1 分钟开始递增，上限 1 小时；新事件和服务重启不能跳过冷却。当前 GitHub 适配器未提供响应头，因此尚不按 `Retry-After` 或额度重置时间精确调度。

相同 `X-GitHub-Delivery` 和相同内容不会重置重试时间或再次打开已完成记录；同一 ID 换成不同正文或事件类型会返回 `409`。已完成记录按 `history_retention_days` 清理，默认保留 30 天；未完成记录一直保留。超过保留期后再投递旧 ID，可能再次刷新，但仍以实时 GitHub 状态生成变化。

GitHub 不会自动重投失败的 Webhook。连接失败、签名错误或服务返回 `503` 时，应修复原因，再在 GitHub Recent deliveries 中手动重投；已经返回 `202` 的后台刷新失败由本地队列重试。程序不另设 GitHub delivery API 轮询。参见 [GitHub 失败投递说明](https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries)与[接收建议](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks)。

## 排查与切回轮询

| 现象 | 检查与恢复 |
| --- | --- |
| `404` | 核对模式、POST 路径及反向代理转发；轮询模式不提供此接口。 |
| `400`／`415` | 核对投递 ID、JSON 内容类型和请求是否被代理改写。 |
| `401` | 核对 GitHub 与本地的密钥；密钥变更后重启服务，不要关闭签名检查。 |
| `503` | 检查密钥、数据库连通性及是否已迁移；恢复后重投未确认的投递。 |
| `202`，但没有变化 | 核对事件是否匹配配置范围、快照是否本来就没有变化，以及 `webhooks.pending`／`webhooks.retrying` 和采集日志。 |
| 一直没有上游更新 | 核对上游是否也配置了 Webhook；目标 fork 的订阅不会代替上游订阅。 |

切回时停止服务，将 `github_source` 改为 `polling`，按需恢复 `discover_notifications`，执行 `check-config` 后重新启动。现有快照和处理记录继续使用；Webhooks 接收接口关闭，已经保存的 Webhook 队列暂停处理，下次切回 Webhook 时继续。GitHub 侧应停用对应订阅，避免持续产生失败投递。

本地测试覆盖 HTTP 验签、数据库去重、重启恢复、退避、范围路由和维护计时器；部署后仍须用实际 GitHub 投递验证 HTTPS、签名和订阅范围。
