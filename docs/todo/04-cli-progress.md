# TODO-04 — CLI 进度条

> 状态:可选(半小时级)
> 来源:PLAN.md 后续计划第 4 项

## 现状

数据链路已就位:dav 层 prog 回调 → `Transfer.BytesDone/BytesTotal`,200ms 节流。本项纯 `cmd/kistctl` 表现层。

## 设计点

- 聚合单行渲染(避免多行 ANSI 刷屏)
- TTY 检测;非终端降级为阶段行输出
- 上传进度按阶段内百分比——总数在加密完成后切换会导致整体百分比回落,必须处理
