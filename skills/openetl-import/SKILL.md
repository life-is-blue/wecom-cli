---
name: openetl-import
description: openetl 批量导入：把旧系统或客户清单（NDJSON、CSV、MySQL）暂存为导入作业、查看比对结果（新增、变更、冲突、可疑、缺失）、在人工确认后合并或整批回滚。当用户要导入或同步工作流清单、核对导入差异、确认合并或撤销一次导入时使用。执行前必须先读取 openetl-shared 完成前置检查。
---

# openetl 批量导入

> 先读取并完成 `openetl-shared`（CLI、授权、写操作规则、输出约束）。本技能只给出导入流程。

导入分三步：**暂存**（不改任何工作流）→ **比对**（读结果、做判断）→ **合并**（管理员，必须人工确认）。暂存和比对需要编辑级别，合并和整批回滚需要管理级别；没有权限时告知用户，不要换方式绕过。

## 什么时候暂存

- 用户给出清单文件（NDJSON、CSV）或旧库连接，要把工作流批量录入或更新。
- 周期性同步：同一个来源再次暂存，比对会标出新增、变更、缺失。
- 只改少量工作流时不要用导入，直接用 `openetl-workflow` 的单条命令。

## 步骤 1：准备映射并预演

映射文件 `mapping.yaml` 把来源列对应到标准字段（名称、domain、area、application、priority、category、来源引用、负责人、阶段、任务）。格式见仓库的 `docs/cli-import.md`。**映射由用户确认**，不要凭列名猜测含义。

```bash
openetl import stage --from ./clients.csv --map mapping.yaml --dry-run
```

`--dry-run` 只在本地读取并映射，输出行数和样例，不发送任何数据。检查样例里的名称、标签、来源引用是否合理，再去掉 `--dry-run`。

来源写法：

- 文件：`--from rows.ndjson`、`--from rows.csv`，标准输入 `--from -`。
- MySQL（只读扫描）：`--from mysql://readonly@host:3306/legacy`，表或查询写在映射的 `source` 里。**密码只通过环境变量 `OPENETL_SOURCE_PASSWORD` 提供**，不要写进命令行、映射文件或回复里，也不会发送到网关。

## 步骤 2：暂存

```bash
openetl import stage --from mysql://readonly@host:3306/legacy --map mapping.yaml --source-system legacy-pipeline
```

返回作业编号（`name` 末段，如 `17`）。重试请带同一个 `--request-id`，避免重复暂存。数据量超过 `--max-rows`（默认 10 万）时分片暂存，每片一个作业。

## 步骤 3：读比对结果

```bash
openetl import show 17
openetl import diff 17 --limit 10
```

`diff` 输出：`counts`（每类行数）、`needs_review`、按优先顺序排列的 `classes`（每类 `count`、前 N 行、是否 `truncated`）。各类含义和处理：

| 类别 | 含义 | 处理 |
| --- | --- | --- |
| `CONFLICT` | 来源与当前数据在同一字段上互相冲突（例如人工改过的值又被来源改写） | **必须人工逐条确认**，不得自行取舍 |
| `SUSPECT` | 匹配不确定或数据异常（疑似重复、名称相近但来源引用不同等） | **必须人工确认**，不得自行判断 |
| `CHANGED` | 已匹配到工作流，来源字段有变化 | 抽查 `changes`，确认变化符合预期 |
| `NEW` | 当前没有对应工作流，将新建 | 抽查名称与标签 |
| `MISSING` | 当前有、来源里没有 | 只提示，导入不会删除；是否下线由用户决定 |
| `UNCHANGED` | 无变化 | 只看数量；需要时 `--action UNCHANGED` |

看某一类的更多行：`openetl import diff 17 --action CONFLICT --limit 50`；翻页原始结果：`--raw --filter 'action = CONFLICT' --page-size 100 --page-count 5`。

向用户汇报：先给每类数量，再列 `CONFLICT`、`SUSPECT` 的行（工作流编号或来源引用、名称、涉及的字段旧值与新值），最后概述 `CHANGED` / `NEW` 的抽样。**不展示内部标识**（遵守 openetl-shared 的输出约束）。

## 步骤 4：合并（必须人工确认）

**绝不在没有人工确认的情况下执行 `import apply`。** 尤其是 `needs_review` 为 `true`（有 `CONFLICT` 或 `SUSPECT`）时：

1. 把冲突和可疑行逐条讲清楚，等用户明确回答"确认合并"或给出处理方式（例如先修正来源数据后重新暂存）。
2. 用户的确认必须针对这一次作业、这一份比对结果；比对之后来源又变了就重新暂存、重新比对。
3. 先预演，再执行：

```bash
openetl import apply 17 --dry-run
openetl import apply 17 --yes --note "客户确认 2026-10-31 清单"
```

`--yes` 表示"人工已确认"，Agent 只有在用户明确确认后才可加上；没有 `--yes` 且不在终端中，命令会以退出码 2 拒绝执行。命令会等待作业完成并输出最终状态；`FAILED`（退出码 1）时把错误原样告知用户，不要重试 `apply`。

## 回滚

合并后发现问题，整批撤销（同样需要用户明确确认）：

```bash
openetl import rollback 17 --dry-run
openetl import rollback 17 --yes --note "清单有误，整批撤销"
```

回滚的是整个作业，不能挑选单行。回滚后再用 `openetl import show 17` 确认状态为 `ROLLED_BACK`。

## 常见错误

- `UNIMPLEMENTED`（HTTP 501）：当前服务端不会主动返回它，只可能来自过旧的服务端版本或前置代理；告知用户检查部署，不要重试。
- `PERMISSION_DENIED`：账号不是管理员或所属方无权，告知用户。
- 映射报错（退出码 2 或 1，信息中带来源行号）：把行号和原因告诉用户，修正映射或来源数据后重试；不要自行改数据。
