---
name: openetl-workflow
description: openetl 工作流进度操作：查看我负责的未完成工作流、开始工作、推进阶段、记录问题或风险评论（可带附件）、回退阶段、调整范围、创建或修改工作流与任务，以及生成日报。当用户要查询或更新迁移工作流进度、写日报、汇报阻塞时使用。执行前必须先读取 openetl-shared 完成前置检查。
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

`accounts/alice` 用 `openetl me` 返回的 `account`。`--filter` 里两个条件之间必须显式写 `AND` 或 `OR`，只用空格并列会报 `AMBIGUOUS_FILTER`（没有隐式 AND）；同一层混用 AND 与 OR 也会报同样的错，请加括号，例如 `(a = 1 AND b = 2) OR c = 3`。按阶段和健康度总览：

```bash
openetl query "SELECT code, name, current_stage, health, open_issues, open_risks FROM workflows WHERE scope = 'in' AND current_stage IS NOT NULL ORDER BY health DESC, current_stage" --page-count 5
openetl query "SELECT stage, health, workflows FROM stage_distribution"
```

「未完成」指 `scope = 'in' AND current_stage IS NOT NULL`（范围内且还有没通过的阶段）。要找「活最多」的工作流，按任务数排序（没有任务的工作流也会出现，任务数为 0）：

```bash
openetl query "SELECT w.code, w.name, w.current_stage, COUNT(t.id) AS task_count FROM workflows w LEFT JOIN tasks t ON t.workflow_id = w.id WHERE w.scope = 'in' AND w.current_stage IS NOT NULL AND JSON_UNQUOTE(JSON_EXTRACT(w.assignees, '$.dev_owner')) = 'alice' GROUP BY w.code, w.name, w.current_stage ORDER BY task_count DESC" --page-count 5
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

预演通过后再执行。需要证据时先 `comment add --attach`，再用 `--evidence <附件资源名>` 引用。
**管理员代对方推进（override）**：规则表把某些阶段留给另一方（例如 `accept` 归客户方）。`admin` 角色可以代对方操作，但**必须带证据**：先上传证据 `openetl comment add WF-000123 --kind note --body "客户邮件确认验收" --attach ./mail.pdf`，从返回里取附件资源名，再执行 `openetl stage advance WF-000123 --stage accept --evidence <附件资源名> --note "客户邮件确认"`。不带证据报 `EVIDENCE_REQUIRED_FOR_OVERRIDE`，错误信息会写明归属的一方。`stage regress`、`scope set` 同样接受 `--evidence`。事件仍记录真实操作人和所属方，并多一条 `override.<键>` 记录，可用 `openetl query "SELECT event_id, actor, side, k, new_v FROM events WHERE k LIKE 'override.%'"` 列出全部代操作。editor 不能代对方操作（`SIDE_NOT_ALLOWED`）。

阶段标记为不适用（NOT_APPLICABLE）需要 `not_applicable` 类 reason，M1 的 CLI 未提供该动作，遇到时告知用户由管理员处理。

## 流程 3：记录问题或风险

问题（`issue`）会使工作流健康度变为 blocked，风险（`risk`）变为 at_risk，提问用 `question`，普通记录用 `note`。评论正文创建后不可修改，写之前先确认内容：

```bash
openetl comment add WF-000123 --kind issue --body "目标库权限未开通，等待客户开通" --dry-run
openetl comment add WF-000123 --kind issue --body "目标库权限未开通，等待客户开通" --attach ./screenshot.png
openetl comment resolve WF-000123 <评论编号> --note "权限已开通"   # <评论编号> 取自 comment add / comment list 返回的 name 最后一段，形如 cm-000130
openetl comment reopen WF-000123 <评论编号>
```

`--attach` 会先上传文件再登记附件，单文件不超过 100 MiB；`--dry-run` 不会上传附件。附件上传失败（例如 `PROJECT_STORAGE_QUOTA_EXCEEDED`、`SIZE_MISMATCH`、`SHA256_MISMATCH`）时不要重试同一文件，把错误告知用户；确需继续时去掉 `--attach`，把证据写进评论正文并说明。

## 流程 4：回退阶段

回退必须带驳回类 `--reason`，可一次回退多个阶段，并建议写 `--note` 说明现象：

```bash
openetl stage regress WF-000123 --stage accept --stage verify --reason rej_row_count --note "行数差 1.2%" --etag <etag> --dry-run
openetl stage regress WF-000123 --stage accept --stage verify --reason rej_row_count --note "行数差 1.2%" --etag <etag>
```

只有当前为 PASSED 的阶段才能回退（否则 `NOT_REGRESSIBLE`）。回退不会自动连带后面的阶段：如果回退后会留下更靠后的 PASSED 阶段，报 `REGRESS_LATER_STAGES_PASSED`，错误信息列出需要一起回退的阶段，请把它们都写进 `--stage`（后面阶段的回退按各自归属方的规则校验，可能需要对方操作）。reason 不确定时，先查有效的原因键，再向用户确认，不要猜：`openetl config get` 任何成员可读，看 `reasons` 里 `kind` 为 `REJECT` 的键（回退用）、`DISCARD`（移出范围用）、`NOT_APPLICABLE`（阶段不适用用）。键写错时报错 `UNKNOWN_REASON` 会直接列出可用的键。回退成功后，该阶段在 `events` 视图里表现为 `k = 'stage.verify'` 的新值 `open`，`reason_key` 列不一定有值，需要原因时看事件的备注。

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

## 流程 7：创建、修改、删除工作流与任务

单个工作流或任务的增删改用下面的命令（批量请用 `openetl-import`）。`--label`、`--annotation`、`--assignee` 每次给出的字段**整体替换**原值：要保留旧标签就把它们一起写上。所有写命令先 `--dry-run`，修改和删除带 `--etag`（先 `get`）：

```bash
openetl workflow create --name daily_loader --label domain=dwh --assignee dev_owner=alice --dry-run
openetl workflow update WF-000123 --name daily_loader_v2 --label domain=dwh --label area=ods --etag <etag> --dry-run
openetl workflow delete WF-000123 --note "重复登记" --etag <etag>
openetl workflow restore WF-000123 --etag <etag>
openetl task create --name extract_orders --workflow WF-000123 --label kind=sql --dry-run
openetl task list --workflow WF-000123 --page-size 100
openetl task assign TK-000456 --workflow WF-000124
openetl task delete TK-000456 --etag <etag>
openetl task restore TK-000456 --etag <etag>
```

每个任务属于且只属于一个工作流：创建时必须给 `--workflow`（工作流必须存在且未删除），`task assign` 表示把任务移到另一个工作流（目标已删除时报 `ENTITY_DELETED`）。删除是软删除，可 `restore` 恢复。删除工作流会在同一事务里连带软删除它的未删除任务（每个任务一条事件，备注写明级联）；恢复工作流只恢复这次被连带删除的任务，之前单独删除的任务仍保持删除。工作流已删除时恢复其任务报 `PARENT_DELETED`，请先恢复工作流。已删除的工作流和它的任务都不会出现在 `workflows` / `tasks` 视图里，评论视图同样不含已删除工作流的评论。
