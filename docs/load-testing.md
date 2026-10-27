# 压测方法

本页负载方案与历史数据针对显式 `AI_MODE=test_fixture` 的流程容量。真实语义模式会调用外部模型并产生用量，需另外评估供应商配额、延迟与费用；请先用 [小规模真实语义用例](real-semantics.md) 验证。

## 测什么

入口返回 202 仅表示受理。必须分别记录 HTTP 受理吞吐与任务完成吞吐，等待队列排空后再判断整条链路。SQLite 与模型模拟器的结果仅适用于当前机器、版本和数据分布。

每次报告记录 commit、Go 版本、CPU/内存、worker 数、数据库路径及初始数据量、持续时间、客户端并发、候选数、故障比例。重启测试使用相同数据库；独立容量测试使用新的测试数据库以免历史数据干扰。

## 运行步骤

1. 启动服务，记录 `/api/health`、`/api/stats` 和 `/metrics` 基线。
2. 用压测客户端提交唯一幂等键，保存每个成功返回的 job_id。
3. 停止增加负载后继续查询这些 job，统计终态、失败原因、UNKNOWN 与排空耗时。
4. 使用重复幂等键、重复恢复调用和重复处理验证无重复互动。
5. 与负载前后预算账本、互动数量和数据库任务数量交叉核对。

客户端仅使用 Python 3 标准库：

先以 `AI_MODE=test_fixture SEED_TENANTS=demo,loadtest` 启动服务，为压测租户创建分身。否则没有候选的请求仅测到理解阶段，不能代表互动链路；脚本会将零候选完成计为验收失败。

```sh
python3 scripts/loadtest.py --base-url http://127.0.0.1:8080 \
  --requests 100 --concurrency 8 --duplicate-every 5 \
  --timeout 120 --poll-interval 0.2 --max-candidates 4 \
  --tenant loadtest --output data/loadtest-report.json
```

默认不注入故障。脚本轮询本次 job 到终态或超时，分别输出受理和端到端延迟，检查重复请求的幂等性及可见互动唯一性。完整故障演示使用 `python3 scripts/demo.py --output data/demo-report.json`。未知结果需对账，不能计入普通完成。

输出还包括 candidate_total、operation_total 和 zero_candidate_completed，用来发现零候选导致的虚高完成吞吐。当前 SQLite 连接池为单连接，hash 向量仅扫描少量预置分身；报告应把这两项写入环境限制。

禁止把尚未执行的测试写成已通过。共享实例的统计还包括其他用户与历史任务；以本次收集的 job_id 作为分母，服务全局计数仅用来交叉核对。

业务 job 终态表示互动发布处理结束，记忆投影由 outbox 异步推进，可能仍有积压。轮询 job 的端到端指标不能标成“所有投影完成”；另外观察 pending outbox 数和记忆投影排空时间。

## 报告最少字段

| 类别 | 字段 |
| --- | --- |
| 受理 | 请求数、202/冲突/限流/其他错误、请求 P50/P95/P99 |
| 完成 | 本次 job 的 COMPLETED/PARTIAL/FAILED/EXPIRED/REJECTED/CANCELLED/UNKNOWN 数 |
| 延迟 | 受理至终态的 P50/P95/P99，明确是否包含排队 |
| 积压 | 发送结束和观察结束时未完成数、排空时间 |
| 正确性 | 幂等请求稳定性、每帖每分身每动作至多一条、删除后可见性 |
| 成本 | 模拟 spent/reserved 增量、每有效互动成本；不得标为真实货币支出 |

## 故障验收

- 相同幂等键与相同正文返回原 ID；不同正文冲突。
- `retry_once`：存在退避和重试记录，最后只产生各动作一次效果。
- `always_fail`：达到重试限制后明确失败，不伪装忽略。
- `comment_reject`：计划含两动作时点赞可成功，评论被拒绝，整项部分完成。
- `unknown_once`：任务进入待对账，普通重试不重复计费/发布；对账后恢复。
- `slow`：处理中停止进程，再启动后租约回收；检查旧执行结果不能覆盖新结果。
- 处理中删除帖子或停用分身：最终提交拒绝失效动作；删除后互动读取不可见。

模拟器无法证明真实审核覆盖、安全过滤、模型质量、远端取消或跨地域灾备。生产验收还需真实供应商契约测试、数据安全评测和真实负载校准。

## 现有运行产物

- [黑盒验收](../reports/acceptance.json)：SIGKILL 后使用原数据库重启、幂等、租户隔离、删除、重试和未知回执对账。
- [100 帖 / 并发 8](../reports/load-100-c8.json) 与 [200 帖 / 并发 32](../reports/load-200-c32.json)：详细基线及测量限制见 [容量文档](capacity-cost.md)。

重新运行黑盒验收前先 `make build`，再执行 `python3 scripts/acceptance.py`。该脚本启动临时数据库与独立服务端口；报告是实际执行证据，不能替代后续代码修改的回归验证。
