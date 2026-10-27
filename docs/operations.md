# 本地运行与恢复

## 配置和数据

默认需要 `AI_BASE_URL`、`AI_API_KEY`、`AI_MODEL` 三个环境变量，详见 [真实语义接入](real-semantics.md)。离线回归显式使用 `AI_MODE=test_fixture`。

服务默认绑定 `127.0.0.1:8080`，SQLite 默认位于 `data/system.db`，默认 4 个 worker。`ADDR`、`DB_PATH`、`WORKERS` 控制对应项。保持数据库路径稳定才能跨进程恢复任务；换一个路径相当于新实例。

`SEED_TENANTS=demo,loadtest` 可为额外演示租户建立分身和预算；`WEB_DIR` 控制静态页面目录。使用新数据库执行独立测试，避免停用分身或撤回记忆影响后续基线。

本地租约约 3 秒并持续续租。进程停止后，未完成任务在租约过期后由 worker 重新领取。阶段结果持久化以减少重复工作；真实外部服务的已执行未回执问题需独立对账，不能由租约解决。

## 操作入口

| 场景 | 入口 | 预期 |
| --- | --- | --- |
| 查看运行状态 | `GET /api/stats`、`GET /metrics` | 队列、状态分布、预算 |
| 查看任务证据 | `GET /api/jobs/{id}` | 阶段、候选、操作和事件 |
| 重试失败任务 | `POST /api/jobs/{id}/retry` | 新 epoch；保留已完成阶段；删除帖子不复活 |
| 真实 AI 未知请求处置 | `POST /api/jobs/{id}/resolve-ai` | 显式放弃并消耗请求额度；随后由用户决定是否新收费重试 |
| 模拟未知结果对账 | `POST /api/jobs/{id}/reconcile` | 查询持久模拟回执，恢复原请求 |
| 停用分身 | `PATCH /api/avatars/{id}`，`{"active":false}` | 授权 epoch 更新，拒绝后续提交 |
| 删除帖子 | `DELETE /api/posts/{id}` | 帖子和互动不可见，停止任务 |
| 编辑帖子 | `PATCH /api/posts/{id}`，包含 expected_version 和新内容 | 版本冲突返回 409；创建新 job，旧版本失效 |
| 查看/撤回记忆 | `GET /api/memories`、`DELETE /api/memories/{id}` | 私有正文隐藏；撤回来源阻止后续提交 |
| 调整演示预算 | `POST /api/budget`，`{"limit":1000}` | 下限为已花费加预留 |

故障注入由发帖的 `fault` 字段选择：`retry_once`、`always_fail`、`unknown_once`、`comment_reject`、`slow`。这些入口只供本地演示，未提供生产管理员授权。

`unknown_once` 模拟评论生成供应商的响应丢失，模拟回执在本地持久保存。真实互动发布仍是 SQLite 事务；这里没有调用外部社交发布 API，不能把此演示当作跨服务恰好一次验证。

## 恢复判断

1. 先查 job、错误和阶段状态，区分业务拒绝、依赖失败与 UNKNOWN。
2. 普通失败使用受控 retry；UNKNOWN 使用 reconcile。不要删除去重记录或清空数据库来“解决”重试。
3. 对组合动作分别核对回执。点赞成功、评论失败时保留点赞；评论恢复不得重复点赞。
4. 验证预算中的 reserved。未知调用预留仍占用属于保守计费，不应直接归零。

删除或编辑后的任务仍可能需要处理历史调用回执。对账可以结算其费用，不能复活已取消或被新版本替代的发布任务。

需要完整保存 SQLite 数据时优先停止服务后备份数据库及其关联文件，或使用 SQLite 在线备份接口。不能在写入期间只复制主数据库文件并假定备份完整。恢复后检查租约、未决操作、outbox 和预算，不直接批量重置终态。

## 生产迁移前检查

真实身份认证、权限管理、安全上传与审核、供应商数据处理约定、真实 embedding/model 接口、分布式持久编排、数据库高可用、向量索引和事件系统均需实现或接入。迁移保持业务身份与逐动作幂等键，先执行历史数据与故障回归，再开放生产发布。

本地页面、HTTP 头、模型模拟器和故障开关都不是生产安全边界。使用公开测试材料，保留本地绑定。

## 监控配置

[Prometheus 配置](../monitoring/prometheus.yml) 和 [告警规则](../monitoring/alerts.yml) 使用当前 `/metrics` 指标。将它们挂载到已有 Prometheus 的 `/etc/prometheus/`，并让 Prometheus 与应用在同一 Docker 网络；本机进程部署时将 target 改为实际可达地址。仓库默认 Compose 只启动应用，不自动启动监控平台。

规则覆盖抓取失败、最老待处理任务、UNKNOWN 未决和 outbox 积压。60 秒、2 分钟、100 条是本地演示阈值，应按冻结的 SLO 和真实负载调整。所有状态计数都是 gauge，不能对其直接使用 counter 的 rate 语义。预算单位是请求额度；真实 token 在 AI 操作账本记录，货币费用仍需供应商账单。

容器验证或端口冲突时可用 `PORT=8083 docker compose up --build`。默认仍绑定本机回环地址，数据放在命名卷内。
