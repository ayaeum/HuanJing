# 交付核对

初版交付范围为允许基础设施替换和外部依赖 Mock 的可运行本地系统。随后已接入真实模型文本理解与候选评分，最新状态及验证见 [真实语义接入](real-semantics.md)；本页其他历史报告按各自版本解释。生产目标设计保留在设计文档中，其余模型能力、分布式基础设施和生产认证的差异列在 README，未作为已实现能力申报。

## 用户交付项

| 要求 | 实际交付 | 验证证据 |
| --- | --- | --- |
| 可运行系统及源码 | Go 服务、SQLite 持久账本、中文 Web 界面 | `go build`、`go test -race ./... -count=1`、`go vet ./...`；[测试输出](../reports/go-test.txt) |
| 自建 Mock 数据和外部依赖 | 五个分身、公开/私有记忆、确定性模型及故障回执，真实 PNG/WAV/MP4 测试附件 | [fixtures](../fixtures/manifest.json)、[八场景演示](../reports/demo-report.json) |
| 系统设计 | 原设计全文、飞书方案和本地替代架构 | [目标设计](design.md)、[本地架构](local-architecture.md) |
| 压测方案 | 闭环生命周期负载、幂等检查、终态与 outbox 分别计量 | [压测文档](load-testing.md)、[脚本](../scripts/loadtest.py) |
| 容量与成本评估 | 最终源码两档本地实测、公式、可调整假设价格计算器 | [容量成本](capacity-cost.md)、[100/c8](../reports/load-relevance-100-c8.json)、[200/c32](../reports/load-relevance-200-c32.json)、[成本情景](../reports/cost-scenario.json) |
| 部署、监控、故障处理 | Dockerfile、Compose、指标及 Prometheus 配置、恢复指引 | [运维](operations.md)、[容器健康状态](../reports/container-status.json)、[容器数据与媒体重启保留](../reports/container-restart.json) |
| README | 启动、演示、能力边界、完成情况、后续计划 | [README](../README.md) |
| 主 Agent 带多个子 Agent 合作 | 后端、界面/演示/负载、文档/对抗审查分别完成，主 Agent 集成验收 | 审查反例已转为后端回归测试；提交按逻辑批次拆分 |
| 推送指定仓库 | `ayaeum/HuanJing` 的 `main` | GitHub 逐提交及远端分支校验 |

## 本地正确性验收

13 个 Go 测试函数覆盖组合动作去重、503 重试、LIKE 成功与 COMMENT UNKNOWN、删除后费用结算、旧 epoch 与记忆撤回、零预算/过期/租户隔离、编辑冻结计划、取消后的 RUNNING 回执、决策来源快照、来源删除与 outbox、候选冻结、全 STALE 失败、四种动作计划及数据库健康探测。具体断言见 [app_test.go](../internal/app/app_test.go)。

[黑盒验收](../reports/acceptance.json) 独立启动临时数据库，实际杀死并重启服务，检查接收幂等、租户隔离、删除、暂时失败及逐个未知回执对账。该证据覆盖单机持久恢复，不证明机房级容灾。

浏览器实际上传 PNG 并发布，检查了阶段、召回证据、LIKE/COMMENT 独立结果和页面无私密 canary；[截图](../reports/ui-desktop.png) 留作展示记录。最终版本重新运行了八场景 HTTP 演示。容器使用独立命名卷运行相同演示，重启后核对帖子、任务、互动数量和上传媒体字节一致。

本轮相关性与访问修复后的源码、二进制哈希及检查见 [E2E 清单](../reports/e2e-verification-manifest.json) 和 [完整 E2E 报告](e2e-validation.md)。原 [验证清单](../reports/verification-manifest.json)、八场景演示和容器重启报告对应前一版本，是历史证据；本轮新增修复通过宿主机真实 HTTP/浏览器套件验证，未重复容器部署验证。两档压测都执行真实持久化与轮询，但模型等待为 30 ms Mock，规模为五个分身，不能推导真实 AI 系统容量或推荐质量。

## 明确保留的生产工作

- 真实视觉/OCR/ASR、审核、个性化决策/评论模型、大规模 embedding 检索和质量/隐私评测。
- Temporal/PostgreSQL/Qdrant/MQ 接入及多节点、高可用与灾备验收。
- 真实身份认证、完整来源授权闭包、供应商 RPM/TPM、成本加权公平调度及细分动作额度。
- 实际供应商账单、外部发布幂等契约、生产 SLO 与真实业务分布压测。

Prometheus 配置提供可接入的规则样例；本次未部署完整监控平台或演练真实告警通知。

GitHub Actions 以 `ci/github-actions.yml.example` 模板交付。当前 GitHub 凭据不含 workflow 写权限，远端 CI 尚未启用；未将其申报为通过。
