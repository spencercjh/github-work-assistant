# 关注私有仓库

默认只关注公开仓库。`private_repositories` 控制私有仓库的通知发现、内容采集和历史上下文访问。程序仍使用服务账号的 `gh` 登录凭据，只执行 GET；配置不会增加 GitHub 权限。

## 配置访问范围

在实例 `config.json` 中设置 `private_repositories`：

| 配置值 | 私有仓库范围 |
| --- | --- |
| `[]` 或省略 | 不允许私有仓库 |
| `["example/project"]` | 一个仓库 |
| `["example/*"]` | 一个组织或用户下的仓库 |
| `["example/project", "another/*"]` | 多个范围的并集 |
| `["*"]` | 当前 GitHub 账号有读取权限的全部私有仓库 |

名称不区分大小写；只支持表中的三种匹配形式，不支持 `*/project`、部分名称通配符或路径跳转。公开仓库继续按原有关注规则处理。

这项配置允许发现范围内的个人 Notifications，并采集已经配置或发现的 Issue／PR。它不会遍历组织内所有仓库，也不会自动订阅所有事项。自动发现还需要 `discover_notifications: true`；固定关注对象仍写在 `watches` 中。

## 从现在开始接收

已有实例启用时，先停止采集服务并备份配置。将下面两项加入现有配置，不要用片段替换整个文件：

```json
{
  "private_repositories": ["example/*"],
  "private_notifications_since": "2026-10-25T12:00:00Z"
}
```

把示例时间换为实际启用时间，可运行 `date -u +%Y-%m-%dT%H:%M:%SZ` 获取。执行 `run check-config` 和 `run doctor` 后重新启动服务。不要回退数据库中的 Notifications 游标。

`private_notifications_since` 是固定起点，重启时不要自动重设。它排除更早更新的私有 Notifications，并限制首次观察时补查评论的范围。首次发现仍会读取事项当前状态作为背景；`baseline` 不表示刚发生的新消息。后续已跟踪事项沿快照比较变化。没有设置该时间时，沿现有游标发现，首次观察评论采用 `lookback_hours` 窗口。

新安装可直接配置范围：

```sh
bin/ghraft init --dir state/assistant \
  --raft-token-file /private/external-token \
  --task-channel '#github-work' \
  --private-repositories 'example/*,another/project'
```

`init` 在启用私有范围时默认使用安装时间作为起点，也可通过 `--private-notifications-since` 指定。交互式 `make setup` 会询问私有范围；留空表示不启用。

## 权限与投递

私有快照和事件标记为 `private`。默认通知只展示变化类别，例如「检查失败」「新增评论」，隐藏仓库名、标题、参与者、链接和正文。执行者从本机 CLI 读取完整内容；需要提醒时，详细信息发送到 `notify_target`，共享任务仅回复处理状态。

如果通知目的地的成员已获准查看私有事项，可在实例 `config.json` 中设置 `private_notification_details: true`。通知将展示仓库名、事项标题、参与者、检查名称、变化摘要和 GitHub 链接，仍不复制评论正文。这个选项只控制 External 模式的展示，不扩大 `private_repositories` 或 GitHub 权限。

更改配置前先停止服务，保留既有私有范围和 `private_notifications_since`；校验配置后重启。新格式用于尚未准备投递的通知。已经准备过的请求保留原来的内容，以便投递失败时安全重试；已经发出的通知不会重新发送。

访问私有历史和重试投递时会重新检查 GitHub 身份及仓库权限。移除私有范围、权限失效或验证失败时，程序停止提供相关旧上下文。它不会因此删除数据库中的事件或已经创建的任务。即使仓库后来公开，先前采集的私有历史仍受原来的私有范围限制。

跨仓库引用仅保留明确公开的来源，并且不复制来源正文。私有或可见性未知的来源继续排除，包括私有事项引用另一个私有事项的情况；来源权限可能独立变化，不能沿用当前事项的授权。

配置授权与内容权限不能替代操作授权：默认仍只读调查，代码修改、评论和发布需要用户另行委托。

## 停用与排查

停用时停止服务，将 `private_repositories` 改为 `[]`，校验配置后重启。私有待发事件保留在本机，恢复范围和权限后可继续处理。已发出的内容不会自动撤回；不要把清空数据库当作停用步骤。

使用当前版本修改范围即可回退到只处理公开事项。旧版没有私有范围和缓存访问检查，不能直接指向已保存私有内容的新数据库。

若配置正确但没有收到通知，检查服务账号是否能执行 `gh api repos/owner/repo --jq '{private, permissions}'`、是否有该事项的 Notifications，以及通知更新时间是否晚于配置起点。`doctor` 核对登录身份和连接，不证明该账号能读取所有私有仓库。`status` 中的采集错误用于区分权限失败与没有新变化；不要将凭据或私有正文粘贴到排查记录中。
