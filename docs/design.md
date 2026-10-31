# Design

The design document lives in the Claude Docs artifact "迁移进度管理平台设计":
https://claude.ai/artifact/42y4JCJcQaEv5rxAAPjMKX

Tabs: main design, "M0 契约草案" (schema, views, rules, HTTP API), "产品化规范与对照" (AIP naming).

Decisions so far: Go for server and CLI in one repo; AIP naming; one instance per project in phase 1;
MySQL 8.0 only (real MySQL in dev and tests); attachments via presigned direct upload (S3-compatible, 100MB cap).
