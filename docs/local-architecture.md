# 本地实现架构

本文件描述可运行版本的组件替换。目标设计见 [design.md](design.md)，原生可编辑画板保留在[飞书方案](https://feishu.cn/docx/GT2DdM2ioo939xxzjaFmFtsHyOh)。本地运行不依赖飞书。

```mermaid
flowchart LR
  UI[浏览器演示界面] --> API[Go HTTP API]
  API --> DB[(SQLite WAL\n帖子 / 任务 / 阶段 / 动作 / 预算)]
  API --> Files[本地上传目录]
  DB --> Workers[有界 Worker\n租约 / epoch / 持久重试]
  Workers --> Audit[Mock 审核与理解]
  Workers --> Match[本地画像召回]
  Workers --> Decision[Mock 个性化决策]
  Decision --> Public[公开上下文评论构造]
  Workers --> Ledger[Mock 调用与回执账本]
  Ledger --> DB
  Public --> Commit[事务校验与逐动作发布]
  Commit --> DB
  DB --> Events[outbox 消费\n派生记忆]
  Events --> DB
  API --> Metrics[Prometheus 指标]
```

```mermaid
sequenceDiagram
  actor User as 用户
  participant API
  participant DB as SQLite
  participant W as Worker
  participant M as 模型 Mock
  User->>API: 帖子 + Idempotency-Key
  API->>DB: 同事务写帖子、job、请求身份
  API-->>User: 202 + job_id
  W->>DB: 领取租约并推进 epoch
  W->>M: 审核、理解、召回后的决策
  M-->>W: 持久结果与模拟回执
  W->>DB: 冻结动作集合
  W->>DB: 校验版本/授权，提交 LIKE
  W->>M: 用公开材料生成 COMMENT
  alt 结果已知且审核通过
    W->>DB: 独立提交 COMMENT 与 outbox
  else 结果 UNKNOWN
    W->>DB: 保留已成功动作及预算预留
    User->>API: 对账请求
    API->>DB: 查询模拟回执，恢复剩余动作
  end
  API-->>User: 逐动作结果和任务状态
```

SQLite 的单机事务承担最终校验与发布，进程内 Worker 执行持久阶段，数据库 outbox 承担事件交付。此实现验证应用协议，不提供 Temporal 的历史回放、PostgreSQL 的多节点部署或 Qdrant 的 ANN 性能。Mock 生成只模拟可重复的结果与故障，不代表真实模型理解、安全审核或推荐质量。
