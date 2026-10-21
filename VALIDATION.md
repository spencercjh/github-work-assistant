# 历史 MVP 验证范围

本页概述此前 MVP 的本机验证。当前候选版本的结果和限制见 [docs/VALIDATION-CURRENT.md](docs/VALIDATION-CURRENT.md)。实例名称、用户身份、Raft 消息标识和本机资源清单保留在原始私有验收材料中。

## 自动化与协议验证

历史版本通过 Go 构建、vet 和启用 race 检查的测试，并使用真实隔离 seekdb 验证快照、事件 outbox 和处理账本持久化。

Raft 协议 fixture 验证了 Agent／Server／App 身份绑定、拒绝携带凭据跟随重定向、HttpOnly cookie 会话、AES-GCM 加密 token、会话撤销与过期、稳定事件 ID 重试及报告幂等。协议 fixture 本身不证明真实事件送达。

## 历史真实接入

历史本机实例曾完成 HTTPS manifest 校验、Agent Login、受保护查询、事件授权、真实 App 事件接收和 Agent 调查报告关联。报告重复记账与 Go 服务重启后读取也已验证。完整回执由部署者私下保留。

HTTPS 调试只对调用进程显式信任证书，没有关闭 TLS 校验。真实 GitHub 操作限于公开对象读取，没有发布评论、review、PR 或代码。

这些结果适用于当时已运行的 Agent 和本机服务。它们不证明当前候选版本已部署、新接收 Agent 已完成授权、离线 Agent 冷启动、长期无人值守可靠性或多用户隔离。

## 调试结论

manifest 注册要求 HTTPS；本机 HTTP 登录演示不能作为 manifest 接入成功的证据。登录回执不等于 action 可用，应实际调用受保护查询核验。事件接收、Agent 执行、报告记账和人类验收也应分别核对。

具体部署使用专用数据库和私有配置，运行资源与源码分开管理。回收前核对服务和数据归属，不能影响同机其他实例。迁移与恢复步骤见 [MIGRATION.md](MIGRATION.md)。
