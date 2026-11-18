# Sub2API 拼车（GPT Pro 20X 车主/车友）定制说明

本仓库是 `Wei-Shaw/sub2api` 的拼车场景定制版。核心能力来自上游内置实现，并在此基础上
补充了**种子迁移 + 后端防护补丁**（空响应体错误透传、兼容路由 failover 兜底、
非 failover 原生 OpenAI 错误透传、全客户端可控指令/消息关键词拦截），前端代码零改动。

- 定制种子：`backend/migrations/251_carpool_seed.sql`（服务启动时自动执行，幂等）
- 种子与补丁测试：`backend/migrations/251_carpool_seed_test.go`、`backend/internal/service/carpool_keyword_bypass_test.go`、`backend/internal/handler/carpool_passthrough_empty_body_test.go`

## 一、能力对照（你提的需求 → 上游实现）

| 需求 | 上游实现 | 定制 |
|---|---|---|
| 车友看限额/过期时间 | API Key 自带 `quota/quota_used/expires_at` + 5h/1d/7d 滚动窗口限流；用户面板 `/api/v1/keys`、`/usage/dashboard/*`、`/user/api-keys/:id/usage/daily` 全部现成 | 无需改动 |
| 车友看使用日志 | 用户端用量列表 `/api/v1/usage` 现成；错误请求页 `/api/v1/usage/errors` 受 `allow_user_view_error_requests` 开关控制（fail-closed，且已脱敏白名单视图） | 种子设为 `false`：车友看不到上游错误明细 |
| 车主添加渠道（标准 API） | 账号体系同时支持 `oauth`（Codex OAuth 等）与 `apikey`（标准 API 接口）两类，管理后台「账号管理」直接添加 | 无需改动 |
| 上游报错伪装 | `ErrorPassthroughService` 兜底规则：命中后用 `custom_message` 改写文案，`response_code` 改写状态码；上游真实错误只进车主的 ops 运维看板（存储时已脱敏凭据字段） | 种子预置 4 条全平台兜底规则；原生 Embeddings/Alpha Search 非 failover 错误、兼容路由 failover 及空响应体均走规则 |
| 违规内容前置过滤、不发上游 | `ContentModerationService`（pre_block + keyword_blocking_mode=keyword_only）：本地关键词同步拦截，命中即 403，**不调用任何外部审核 API、不发上游请求** | 种子预置初始关键词表 + 本地关键词扫描所有客户端可控的 `system`/`developer`/`user` 文本及独立顶层指令 |
| 车友违规治理 | 关键词命中写入 `content_moderation_logs`；可选自动封号（`auto_ban_enabled` + 阈值） | 种子默认只拦不封，车主可开 |

## 二、部署

### 方式 A：Docker Compose（推荐）

```bash
# 1. 准备部署目录
mkdir -p sub2api-deploy && cd sub2api-deploy

# 2. 用本仓库的 compose 文件与 .env（或按官方 docker-deploy.sh 生成）
cp /path/to/this-repo/deploy/docker-compose.local.yml docker-compose.yml
cp /path/to/this-repo/deploy/.env.example .env
mkdir -p data postgres_data redis_data
# 编辑 .env：POSTGRES_PASSWORD / JWT_SECRET / TOTP_ENCRYPTION_KEY 必填
#   openssl rand -hex 32  生成上述三个值

# 3. 镜像：官方镜像不含 251 种子迁移。二选一：
#    a) 本地构建（需要 Docker Buildx，前端+后端全量编译）：
docker build -t sub2api-carpool:latest /path/to/this-repo
#    b) 或推到 GHCR/公仓用 Actions 构建（.github/workflows/release.yml 已有现成流水线，fork 后打 tag 即可）
# 然后把 docker-compose.yml 里的 image 改成你的镜像。

# 4. 启动（迁移在启动时自动应用，包含 251 号种子）
docker compose up -d
docker compose logs -f sub2api
```

启动后打开 `http://服务器IP:8080` 完成设置向导（数据库/Redis 已由 compose 提供）。

### 方式 B：二进制 + 自备 PostgreSQL 15+/Redis 7+

```bash
# 后端（Go 1.27+）：在 backend/ 下
make build        # 或 go build ./cmd/server
# 前端（Node 24+，使用 pnpm 9）：在 frontend/ 下
corepack enable
pnpm install --frozen-lockfile && pnpm run build
```

产物部署后首次启动会自动跑全部 SQL 迁移（含 251 种子）。

### 验证种子已生效

> 注意：251 号种子的大多数 settings 采用 `ON CONFLICT (key) DO NOTHING`，
> 不会覆盖老实例中管理员已经手工保存过的配置；唯一例外是风险总开关
> `risk_control_enabled`，使用 `ON CONFLICT (key) DO UPDATE` 强制回到 `true`，
> 以修复早期 135 号迁移留下的 `false`。

启动后用管理员登录，检查：

1. **风控中心菜单出现**（`risk_control_enabled=true` 控制前端菜单可见性）
2. 「风控中心 → 内容审计」里 mode=pre_block、关键词列表非空
3. 「账号管理 → 工具 → 错误透传规则」里可见 4 条 `carpool-mask-*` 规则
4. 用一个车友 Key 发一条含关键词的消息，应得到 403 + 种子里的 block_message，
   且管理后台运维看板能看到这条本地拦截记录（不上游）

## 三、车主日常操作

### 维护关键词黑名单

管理后台「风控中心 → 内容审计 → 屏蔽关键词」文本框直接增删（每行一个，最多 10000 条，
单条 200 字）。保存后约 1 秒内热生效，无需重启。当前策略：

- mode=`pre_block`：请求进来先同步检查
- keyword_blocking_mode=`keyword_only`：只跑本地关键词，不调用外部审核模型
  （因此无需在「审核 API Key」里填任何东西）
- sample_rate=100：关键词检查永不抽样跳过

如需更聪明的语义级审核（如 Qwen3Guard），可在管理后台「提示词审计」页接入任意
OpenAI 兼容端点，并把 `keyword_blocking_mode` 调回 `keyword_and_api`——这是纯配置操作。

### 维护报错伪装

管理后台「账号管理 → 工具 → 错误透传规则」。种子规则的语义：

| 规则 | 匹配 | 车友看到 |
|---|---|---|
| carpool-mask-5xx | 500/502/503/504/529 | 502 「上游服务暂时不可用，请稍后重试；若持续失败请联系管理员。」 |
| carpool-mask-429 | 429 | 502 「上游服务暂时不可用，请稍后重试；若持续失败请联系管理员。」 |
| carpool-mask-auth | 401/402/403 | 502 「上游服务暂时不可用，请稍后重试；若持续失败请联系管理员。」 |
| carpool-mask-400 | 400/404/413/422 | 502 「上游服务暂时不可用，请稍后重试；若持续失败请联系管理员。」 |

所有规则的 `skip_monitoring=false`：真实上游错误（含脱敏后的响应体）始终记录在车主的
运维看板，车友端拿到的只有 custom_message。

### 给车友发 Key

「API Key 管理」（用户自助）或车主代建时设置：

- `quota`（美元额度，0=不限）与 `expires_at`（过期时间）——车友在面板可见
- `rate_limit_5h/1d/7d`（滚动窗口美元限额）——防止单人打爆
- 分组绑定决定可用渠道与倍率

### 违规处置

- 默认只拦截并记录（`auto_ban_enabled=false`）
- 想自动封号：管理后台打开「自动封禁」，`ban_threshold` 次命中（统计窗口
  `violation_window_hours`）即禁用账号（管理员账号豁免）
- 「风控中心 → 审计日志」可查每次命中：用户、Key、命中的关键词、输入摘录

## 四、与上游同步

定制包含以下新增与微调文件，rebase 上游 `main` 时仅需留意后端小改点与工具链文件：

- 新增文件：
  - `CARPOOL.md`
  - `backend/migrations/251_carpool_seed.sql`
  - `backend/migrations/251_carpool_seed_test.go`
  - `backend/internal/service/carpool_security.go`
  - `backend/internal/service/json_security.go`
  - `backend/internal/service/json_security_test.go`
  - `backend/internal/service/carpool_keyword_bypass_test.go`
  - `backend/internal/handler/carpool_json_security_test.go`
  - `backend/internal/handler/carpool_passthrough_empty_body_test.go`
- 后端定制改动：
  - `backend/cmd/server/VERSION`
  - `backend/internal/handler/openai_gateway_handler.go`（`applyOpenAIFailoverPassthroughRule` 优先于凭据失败返回，允许空响应体命中规则；OpenAI 账号反代 Anthropic Messages 时接入同一透传规则）
  - `backend/internal/handler/gateway_handler.go`（`matchFailoverPassthroughRule` 抽取共用逻辑并允许 failover 耗尽透传空响应体）
  - `backend/internal/handler/gateway_handler_responses.go`（Anthropic 组 `/v1/responses` failover 耗尽优先匹配透传规则）
  - `backend/internal/handler/gateway_handler_chat_completions.go`（Anthropic/兼容组 `/v1/chat/completions` failover 耗尽优先匹配透传规则；仅发心跳后补发错误帧与 `[DONE]`）
  - `backend/internal/handler/gemini_v1beta_handler.go`（Gemini failover 耗尽透传允许空响应体）
  - `backend/internal/handler/security_audit_helper.go`（全局拦截重复 JSON 键，杜绝解析差异绕过）
  - `backend/internal/service/content_moderation.go`、`backend/internal/service/content_moderation_input.go`（新增 Embeddings 与 Alpha Search 协议；拦截 Embeddings 整数 token 数组输入；关键词扫描所有客户端可控的 `system`/`developer`/`user` 文本、Responses `instructions`、Anthropic 顶层 `system`、Gemini `systemInstruction`/`system_instruction`；语义审核仍保持最新 `user` 行为）
  - `backend/internal/service/openai_embeddings.go`、`backend/internal/handler/openai_embeddings.go`（非 failover 错误优先匹配透传规则，未命中时统一 502 兜底伪装并记录 ops 真实错误）
  - `backend/internal/service/openai_alpha_search.go`、`backend/internal/handler/openai_alpha_search.go`（独立 Alpha Search 与 Responses fallback 非 failover 错误透传规则与未命中统一 502 兜底伪装）

> 注意：迁移文件有 SHA256 校验。如果未来你手工改了已在线上环境执行过的 251 号种子，
> 会触发 checksum mismatch；正确做法是改关键词走管理后台（settings 表），或新增 252 号迁移。

## 五、已知边界

- 关键词提取与匹配边界：
  - 本地关键词审核会全面扫描当前请求中客户端可控的所有 `system`/`developer`/`user` 消息文本，以及 OpenAI Responses `instructions`、Anthropic 顶层 `system`、Gemini `systemInstruction`/`system_instruction`，以及 Alpha Search 的 `commands` 与 `input` 文本；因此头部指令、中间历史 user、开头 user、搜索词和工具循环末尾的顶层指令都不会绕过拦截。
  - `assistant`/`model`/`tool`/`tool_result`/`functionResponse`/`function_call_output` 等非客户端指令结果文本仍排除，避免把模型输出或工具回传误判为车友输入；语义审核的“最新 user”边界保持上游行为不变。Embeddings 同时检查 `input` 的单个字符串和字符串数组；整数 token 数组输入直接拒绝，防范绕过。
  - 全局前置校验拒绝冲突重复 JSON 键（RFC 8259 规范，同值重复键放行以兼容合法客户端），杜绝网关与上游解析器不一致引发的走私绕过漏洞。
  - 匹配为大小写不敏感的子串匹配（`strings.Contains`），在后台追加短关键词时注意避免误伤正常语义。
- 上游报错伪装边界：
  - 所有预置 `carpool-mask-*` 规则默认统一改写为 `502` 与同一条通用文案，不对外区分上游错误类型；若后续希望放开 `429` 或 `400` 类原状态码，可在后台对应规则中开启「透传状态码」。
  - 凭据类 failover 失败及流式失败帧优先匹配透传规则，统一伪装为 502 通用文案，避免暴露内部账号体系与 OAuth 凭据信息。
  - 原生 Embeddings 与 Alpha Search 在非 failover 出现上游错误时，若规则未匹配，自动回退至统一 502 兜底伪装文案并写入 ops 日志，确保上游真实错误体绝不直接暴露给客户端。
  - 流式请求中上游**已开始输出正文后**的中断无法伪装（SSE 已发出 200），种子规则只覆盖响应头之前（或仅发出 keepalive 心跳未输出正文前）的失败；Chat Completions 在后者会补发错误帧和 `[DONE]`。
  - 空响应体且带 HTTP 状态码的上游失败现已可命中按状态码匹配的规则（含原生路由及 `/v1/responses`、`/v1/chat/completions` 兼容路由 failover 耗尽）；无 HTTP 状态码且无 `LastFailoverErr` 的本地调度/连接失败仍走通用 502 兜底文案。
  - 若未配置或未命中透传规则，凭据/访问态拒绝、过载降级（`IsOpenAICapacityShed`）、兼容模型不存在 400、静默拒绝以及 `cyber_policy` 硬阻断会走上游默认分支；Responses WebSocket 模式则使用内置脱敏关闭文案，不读取 `error_passthrough_rules`。
