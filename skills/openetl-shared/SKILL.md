---
name: openetl-shared
description: openetl 业务技能的公共前置检查与通用输出约束。任何 openetl-* 技能准备执行 openetl 命令前，都必须同时读取本技能，检查 CLI 是否安装、令牌是否可用，并遵守写操作的 --dry-run / etag / request-id 规则和 ID 字段禁止外露约束。本技能不处理具体业务请求。
---

# openetl 公共前置检查

openetl 是迁移进度管理平台：只记录工作流（workflow）和任务（task）的进度，不执行迁移。**每次准备执行任意 `openetl` 命令前，先完成本技能；检查通过后，再回到对应业务技能。**

## Step 1：检查 CLI

```bash
openetl --version
```

- 输出 `openetl <版本>` → 继续 Step 2。
- 命令不存在 → 停止业务操作，告知用户需要先安装 openetl CLI，不要自行猜测安装方式。

## Step 2：检查授权

```bash
openetl auth status
```

- 输出 `"authenticated":true` 且退出码为 0 → 前置检查完成。
- 退出码为 1（`"authenticated":false`）→ 执行 Step 3。

## Step 3：非交互登录

云端 Agent 不要交互输入。优先使用环境变量，无需 `auth login`：

```bash
export OPENETL_BASE_URL=https://openetl.example.com
export OPENETL_PROJECT=acme-dw
export OPENETL_TOKEN=<由用户或密钥管理提供>
openetl auth status
```

本机长期使用时，从标准输入读取令牌并加密保存（`~/.config/openetl/credentials.enc`）：

```bash
printf '%s' "$OPENETL_TOKEN" | openetl auth login --token-stdin --base-url "$OPENETL_BASE_URL" --project acme-dw
```

令牌优先级：`OPENETL_TOKEN` 环境变量 > 本地加密存储。项目：`--project` > `OPENETL_PROJECT` > 已保存的项目。

仍然失败时停止并把错误告知用户。**绝不回显、打印或写入令牌。**

## 命令约定

- 输出：默认单行 compact JSON 到 stdout；`--output FILE` 写文件；列表和查询加 `--page-count N` 自动翻页并输出 NDJSON（每页一行）。
- 错误：stderr 输出 `{"error":{"code","status","message","details"}}`。退出码 `0` 成功，`1` 运行或 API 错误，`2` 用法错误或参数不合法。
- 工作流编号接受 `WF-000123`、`wf-000123` 或完整资源名 `projects/acme-dw/workflows/wf-000123`。
- 子命令组：`workflow`、`task`、`stage`、`scope`、`comment`、`events`、`attachment`、`import`（见 `openetl-import`）、`member`、`token`、`config`（后三者需要管理级别）。
- 只读统计用 `openetl query "<SELECT ...>"`，可用的视图和列用 `openetl views` 查看。

## 写操作规则

1. **先 `--dry-run`**：所有写命令都支持 `--dry-run`（只校验、不写入）。先预演，确认返回结果符合预期，再去掉 `--dry-run` 真正执行。
2. **并发控制 `--etag`**：先 `openetl workflow get <WF>` 读到 `etag`，写入时带 `--etag <值>`。遇到 `ABORTED` / `ETAG_MISMATCH`，说明对象已被他人修改：重新读取、重新判断，不要盲目重试。
3. **幂等 `--request-id`**：默认自动生成 UUID。需要安全重试（超时、网络错误）时，自己生成一个 UUID 并在重试时传同一个值，重复提交只会返回第一次的结果。
4. **没有 `--dry-run` 的写入**：`member`、`token`、`config`、`import stage` 的 `--dry-run` 只在本地展示将发送的请求，不经服务端校验；`import apply` / `rollback` 的 `--dry-run` 才是服务端校验。
5. **原因与备注**：回退、范围调整等需要 `--reason`（取自项目配置的 reason_key）；`--note` 写人能读懂的说明。
6. 失败时按 `error.details[].reason` 处理：`STAGE_ORDER_VIOLATION`（前序阶段未通过）、`SIDE_NOT_ALLOWED`（你所属的一方无权做此变更）、`REASON_REQUIRED`、`NOT_REGRESSIBLE`、`MERGE_CYCLE`。无权限时告知用户，不要换方式绕过。

## 通用输出约束：内部标识禁止外露

本约束对所有 `openetl-*` 技能生效，优先级高于各业务技能的输出格式。

- **禁止**在最终回复中出现内部标识：`uid`、`storage_key`、`page_token` / `next_page_token`、`etag`、`request_id`、数据库自增编号（`id`、`entity_id`、`workflow_id` 等视图中的整数列）。它们只用于后续命令调用。
- **必须**使用可读信息：工作流编号（如 `WF-000123`，可展示）和 `display_name` / `name`、阶段名称、账号显示名、评论摘要、时间。
- 需要用户在多个候选里选择时，用序号 + 名称 + 当前阶段构造列表，不用内部标识让用户辨认。
- 用户直接索要内部标识时，说明属于内部字段，改用可读信息继续完成操作。
- 令牌属于最高敏感信息，任何情况下都不展示；`openetl token create` 返回的 `secret` 只显示一次，交给用户本人保管，不要写入回复、日志或仓库文件。
- 旧库密码（`OPENETL_SOURCE_PASSWORD` 或连接串中的密码）同样不展示、不写入文件。
