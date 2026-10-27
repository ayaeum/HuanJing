# 本地实现架构

本文件描述可运行版本的组件替换。目标设计见 [design.md](design.md)，原生可编辑画板保留在[飞书方案](https://feishu.cn/docx/GT2DdM2ioo939xxzjaFmFtsHyOh)。本地运行不依赖飞书。

```mermaid
flowchart LR
  UI[浏览器演示界面] --> API[Go HTTP API]
  API --> DB[(SQLite WAL\n帖子 / 任务 / 阶段 / 动作 / 预算)]
  API --> Files[本地上传目录]
  DB --> Workers[有界 Worker\n租约 / epoch / 持久重试]
  Workers --> Audit[规则模拟审核]
  Audit --> Understand[真实模型文本理解]
  Understand --> Match[真实模型公开画像评分]
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
  participant M as 真实语义模型
  participant C as 模拟决策与评论
  User->>API: 帖子 + Idempotency-Key
  API->>DB: 同事务写帖子、job、请求身份
  API-->>User: 202 + job_id
  W->>DB: 领取租约并推进 epoch
  W->>M: 文本理解 / 全量公开画像评分
  M-->>W: 实际结构化响应与 Token 用量
  W->>DB: 冻结动作集合
  W->>DB: 校验版本/授权，提交 LIKE
  W->>C: 用公开材料构造模拟 COMMENT
  alt 结果已知且审核通过
    W->>DB: 独立提交 COMMENT 与 outbox
  else 结果 UNKNOWN
    W->>DB: 保留已成功动作及预算预留
    User->>API: 对账请求
    API->>DB: 查询模拟回执，恢复剩余动作
  end
  API-->>User: 逐动作结果和任务状态
```

SQLite 的单机事务承担最终校验与发布，进程内 Worker 执行持久阶段，数据库 outbox 承担事件交付。此实现验证应用协议，不提供 Temporal 的历史回放、PostgreSQL 的多节点部署或 Qdrant 的 ANN 性能。文本理解与公开画像语义评分使用真实模型 API；审核、动作决策和评论生成继续模拟。真实 AI 的 ai_operations 与模拟 operations 独立记账，真实 UNKNOWN 不能通过模拟回执对账。

## 相关性与附件访问

召回先读取真实模型的摘要、意图和主题，再让真实模型逐一评价当前租户全部启用的公开画像（最多 50 个）。返回的 ID 必须属于请求快照，评分须在 0..1，所有画像须恰好评价一次，相关画像须提供帖子与画像的原文证据。按相关性布尔值和分数阈值筛选，再按分数截取候选。没有向量数据库或字符哈希兜底；大规模检索仍待实现。详见 [真实语义接入](real-semantics.md)。

上传记录与帖子绑定历史保存在 upload_bindings；帖子新增/编辑触发器在同一事务保存引用。未绑定文件可预览；曾绑定文件需要至少一个未删除且当前仍引用该文件的帖子才能读取。编辑移除和删除最后一个父帖会关闭访问，多个父帖共享文件时保留有效引用。启动时迁移现有帖子引用；旧版本已丢弃的历史引用无法从当前表恢复。文件物理清理尚未实现。
