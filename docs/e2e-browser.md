# 真实浏览器端到端验收

## 范围与证据

浏览器测试通过 Ego Lite 的真实页面控件发起请求，使用独立 `127.0.0.1:8084` 服务和 SQLite 测试库。8080 演示数据保持独立。测试读取 DOM 和对应 API 的权威状态作自动断言；模型、媒体理解、审核和向量语义仍是 LOCAL_MOCK，测试通过不能推导真实模型质量或生产吞吐。

- 逐项结果、耗时、帖子/任务 ID、后端二进制 SHA-256：[e2e-browser.json](../reports/e2e-browser.json)
- 桌面截图：[e2e-browser-desktop.png](../reports/e2e-browser-desktop.png)
- 窄屏截图：[e2e-browser-mobile.png](../reports/e2e-browser-mobile.png)
- 可执行浏览器脚本：[e2e-browser.mjs](../scripts/e2e-browser.mjs)
- 启动包装器：[run-browser-e2e.py](../scripts/run-browser-e2e.py)

自动断言与视觉审查分开记录。报告 `cases` 表示自动断言，`visual_review` 表示对截图的人工或模型目视检查。

本轮最终执行于 **2026-10-28 01:57:16–01:58:13（Asia/Shanghai）**，22 个自动场景全部通过。后端二进制 SHA-256 为 `b63168639592e2ed0a4f36d174b338ae2972f7a2d9b1f3bcd4a4f5b49dd37fa5`，含最终召回资格及附件引用修复。桌面和移动首屏截图已单独目视检查；移动端本轮覆盖宽度断言和首屏布局，未声称完成所有移动端交互回归。结束后关闭测试空间与 8084 服务，保留用户的 8080 演示服务。

## 从干净环境复现

依赖 Go、Python 3 和已安装可用的 `ego-browser`。在项目根目录执行：

```bash
go build -o /tmp/huanjing-browser-e2e ./cmd/server
ADDR=127.0.0.1:8084 \
DB_PATH=data/browser-e2e-new-run.db \
SEED_TENANTS=demo \
WORKERS=4 \
/tmp/huanjing-browser-e2e
```

另一个终端执行：

```bash
python3 scripts/run-browser-e2e.py \
  --base-url http://127.0.0.1:8084 \
  --server-binary /tmp/huanjing-browser-e2e
```

每轮使用新的数据库路径，确保画像处于初始启用状态、预算充足。包装器显式传入项目根目录，因为 Ego Node 运行时的默认工作目录可能为 `/`。全部场景完成后脚本保存报告，零失败时关闭测试浏览器空间；任何断言失败时返回非零退出码并保留空间供检查。

如果测试中断，先确认已分发的浏览器脚本确实结束，再用同一空间 `--space-id ID` 继续。**仅给 CLI 发送 SIGINT 不能证明浏览器端执行已停止**，禁止立刻在同一页面并发启动另一轮。后端升级应等当前测试自然结束，再重建、创建新测试库并验证二进制 hash。

## 自动场景与复现步骤

| 编号 | 场景 | 页面操作及断言 |
| --- | --- | --- |
| B01 | 初始状态 | 打开页面，检查“已连接”、Mock 边界、5 个公开画像、无私密 canary |
| B02 | 点赞并评论 | 候选上限 1，发布徒步旅行摄影相关文本；动作严格为 LIKE 与 COMMENT |
| B03 | 仅点赞 | 同主题文本加“只点赞”；仅有 LIKE |
| B04 | 仅评论 | 同主题文本加“只评论”；仅有 COMMENT |
| B05 | 忽略 | 同主题文本加“不需要互动”；有候选、零互动且 UI 显示 IGNORE |
| B06 | PNG | 选择 running.png、填写 Mock 描述并发布；附件存在且图片 naturalWidth > 0 |
| B07 | WAV | 选择 training.wav 并发布；浏览器读取音频元数据和时长 |
| B08 | MP4 | 选择 outdoor.mp4 并发布；浏览器读取视频元数据、尺寸和时长 |
| B09 | 瞬时依赖失败 | 选择 retry_once；先有 FAILED 理解调用，随后成功，最终完成 |
| B10 | 持续失败与手动重试 | 选择 always_fail，等待 FAILED 后点击“受控重试”；epoch 增加，故障未消失时仍明确失败，零发布 |
| B11 | UNKNOWN | 选择 unknown_once，等“等待对账”，点击“查询模拟回执并对账”；完成且评论仅一条 |
| B12 | 评论拒绝 | 选择 comment_reject；结果 PARTIAL_COMPLETED，已计划的点赞保留，评论未发布 |
| B13 | 删除 | 完成后点击删除、确认对话框；帖子读取 410，列表互动不可见 |
| B14 | 停用分身 | 停用山野旅行家，再发相关帖；候选和互动排除该分身，结束时恢复启用 |
| B15 | 刷新恢复 | slow 场景受理后刷新，重新选择同一帖子；原任务最终完成 |
| B16 | 提交响应丢失 | 浏览器 fetch 包装器在服务已提交后丢弃响应；sessionStorage 保留键和 body，刷新后用“重试确认上次提交”恢复，同一帖子与任务，成功后清理待确认记录 |
| B17 | 快速双击 | 对发布按钮连续两次原生 click；禁用态阻止第二次请求，只创建一次 |
| B18 | HTML 转义 | 发布带 img/onerror 的字符串；作为文本展示，无注入节点、无脚本执行、无 canary 泄露 |
| B19 | 媒体证据缺失 | 上传 PNG，Mock 描述留空；审核拒绝且零互动 |
| B20 | 运行异常 | 检查页面 error/unhandledrejection；记录为空。刷新会重装探针，各场景仍独立断言 |
| B21 | 窄屏布局 | 切换 390×844 视口，文档无横向溢出；截图另行视觉检查 |
| B22 | 纯马拉松相关性 | 候选上限 4，发布纯马拉松配速/恢复内容；候选严格仅 fitness，互动也仅来自 fitness |

四种动作短语仅是确定性 Mock 的测试开关，不是生产模型策略。媒体文件是有效测试图、正弦音和动态图，Mock 描述不代表真实 OCR/ASR/视频分析。

## 本轮发现与修正

测试发现成功上传后缺少直接清除已选附件的入口，已增加“移除全部附件”按钮，并在测试转回纯文本时真实点击该按钮。测试不依赖 `setInputFiles([])`：当前 Ego 运行时该调用未清除已选文件，会使后续纯文本案例误携带旧媒体并触发证据缺失拒绝。

早期中断轮存在两个测量问题：附件未清除，以及中断 CLI 后旧浏览器脚本继续运行、干扰新轮。相关记录保留于 `e2e-browser-initial.json`、`e2e-browser-interrupted.json`，**不计入产品最终通过率**。最终结论仅使用源码冻结、单一脚本、独立新库及明确二进制 hash 的完整轮。

## 结论填写规则

最终自动通过数取 `reports/e2e-browser.json` 的 `passed/failed`，视觉检查结论取同文件 `visual_review`。报告缺少 `finished_at` 或存在失败时，此轮尚未完成验收。浏览器验证不替代 API 权限测试、SIGKILL 恢复、跨租户隔离、长期负载和生产依赖验证。
