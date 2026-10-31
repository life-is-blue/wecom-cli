---
name: openetl-workflow
description: openetl 工作流进度操作：查看我负责的未完成工作流、开始工作、推进阶段、记录问题或风险评论（可带附件）、回退阶段、调整范围，以及生成日报。当用户要查询或更新迁移工作流进度、写日报、汇报阻塞时使用。执行前必须先读取 openetl-shared 完成前置检查。
---

# openetl 工作流进度

> 先读取并完成 `openetl-shared`（CLI、授权、写操作规则、输出约束）。本技能只给出业务流程和命令。

阶段顺序：`development` 开发 → `deployment` 部署 → `verify` 校验 → `accept` 验收 → `dual_run` 双跑 → `cutover` 割接。前序阶段未 PASSED 或 NOT_APPLICABLE 时，后续阶段不能推进。验收（`accept`）通常由客户方推进。

先用 `openetl views` 确认可查询的视图和列。

## 流程 1：开始工作，找出我名下未完成的工作流

用 `events` 视图或负责人信息定位我的工作；最稳妥的是先列出我作为负责人的工作流：

```bash
openetl me
openetl workflow list --filter 'assignees.dev_owner = "accounts/alice" AND scope = IN_SCOPE' --page-size 100 --page-count 5
```

`accounts/alice` 用 `openetl me` 返回的 `account`。按阶段和健康度总览：

```bash
openetl query "SELECT code, name, current_stage, health, open_issues, open_risks FROM workflows WHERE scope = 'in' AND current_stage IS NOT NULL ORDER BY health DESC, current_stage" --page-count 5
openetl query "SELECT stage, health, workflows FROM stage_distribution"
```

「未完成」指 `scope = 'in' AND current_stage IS NOT NULL`（范围内且还有没通过的阶段）。要找「活最多」的工作流，按任务数排序（没有任务的工作流也会出现，任务数为 0）：

```bash
openetl query "SELECT w.code, w.name, w.current_stage, COUNT(t.id) AS task_count FROM workflows w LEFT JOIN tasks t ON t.primary_workflow_id = w.id WHERE w.scope = 'in' AND w.current_stage IS NOT NULL AND JSON_UNQUOTE(JSON_EXTRACT(w.assignees, '$.dev_owner')) = 'alice' GROUP BY w.code, w.name, w.current_stage ORDER BY task_count DESC" --page-count 5
```

查询不支持子查询和 EXISTS，需要时用 JOIN 加 GROUP BY。健康度在视图里是小写 `ok|at_risk|blocked`，在 `workflow get` 的 JSON 里是 `NORMAL|AT_RISK|BLOCKED`，含义相同。

逐个查看详情并取得 `etag`：

```bash
openetl workflow get WF-000123
openetl comment list WF-000123 --filter 'resolved = false'
```

向用户汇报时列出编号、名称、当前阶段、健康度和未解决事项，不展示内部标识。

## 流程 2：推进阶段

```bash
openetl stage advance WF-000123 --stage development --note "代码已合并" --etag <etag> --dry-run
openetl stage advance WF-000123 --stage development --note "代码已合并" --etag <etag>
```

预演通过后再执行。需要证据时先 `comment add --attach`，再用 `--evidence <附件资源名>` 引用。阶段标记为不适用（NOT_APPLICABLE）需要 `not_applicable` 类 reason，M1 的 CLI 未提供该动作，遇到时告知用户由管理员处理。

## 流程 3：记录问题或风险

问题（`issue`）会使工作流健康度变为 blocked，风险（`risk`）变为 at_risk，提问用 `question`，普通记录用 `note`。评论正文创建后不可修改，写之前先确认内容：

```bash
openetl comment add WF-000123 --kind issue --body "目标库权限未开通，等待客户开通" --dry-run
openetl comment add WF-000123 --kind issue --body "目标库权限未开通，等待客户开通" --attach ./screenshot.png
openetl comment resolve WF-000123 <评论编号> --note "权限已开通"   # <评论编号> 取自 comment add / comment list 返回的 name 最后一段，形如 cm-000130
openetl comment reopen WF-000123 <评论编号>
```

`--attach` 会先上传文件再登记附件，单文件不超过 100 MiB；`--dry-run` 不会上传附件。服务端若返回 `UNIMPLEMENTED`（501），说明当前部署尚未开放附件，去掉 `--attach`，把证据写进评论正文，并告知用户。

## 流程 4：回退阶段

回退必须带驳回类 `--reason`，可一次回退多个阶段，并建议写 `--note` 说明现象：

```bash
openetl stage regress WF-000123 --stage accept --stage verify --reason rej_row_count --note "行数差 1.2%" --etag <etag> --dry-run
openetl stage regress WF-000123 --stage accept --stage verify --reason rej_row_count --note "行数差 1.2%" --etag <etag>
```

只有当前为 PASSED 的阶段才能回退（否则 `NOT_REGRESSIBLE`）。reason 不确定时，先查有效的原因键，再向用户确认，不要猜：`openetl config get` 任何成员可读，看 `reasons` 里 `kind` 为 `REJECT` 的键（回退用）、`DISCARD`（移出范围用）、`NOT_APPLICABLE`（阶段不适用用）。键写错时报错 `UNKNOWN_REASON` 会直接列出可用的键。回退成功后，该阶段在 `events` 视图里表现为 `k = 'stage.verify'` 的新值 `open`，`reason_key` 列不一定有值，需要原因时看事件的备注。

## 流程 5：调整范围

```bash
openetl scope set WF-000200 --to out --reason merged --into WF-000123 --dry-run
openetl scope set WF-000200 --to in --note "恢复迁移"
```

移出范围需要 discard 类 reason；`merged` 还必须带 `--into`。恢复范围不会清除已有阶段状态。

## 流程 6：生成日报

1. 总览：`openetl query "SELECT stage, health, workflows FROM stage_distribution"`。
2. 今日进展（`events` 视图一行一个键变更，`k` 形如 `stage.development`，时间用 `event_time`）：

```bash
openetl query "SELECT w.code, e.k, e.new_v, e.actor, e.event_time FROM events e JOIN workflows w ON w.id = e.entity_id WHERE e.k LIKE 'stage.%' AND e.event_time >= '2026-10-31 00:00:00' ORDER BY e.event_time" --page-count 5
```

3. 阻塞项：

```bash
openetl query "SELECT code, name, current_stage, open_issues, open_risks FROM workflows WHERE health <> 'ok' AND scope = 'in'" --page-count 5
```

4. 把 `entity_id` 对应成工作流编号和名称后再汇报（查询时直接选 `code`、`name`，或用 `openetl workflow get`）。日报结构：今日完成、进行中、阻塞与风险（含负责方）、明日计划。**日报中只出现编号、名称、阶段、时间和人名，不出现内部标识。**
