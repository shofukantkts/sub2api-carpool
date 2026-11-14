# Sub2API 拼车（GPT Pro 20X 车主/车友）定制说明

本仓库是 `Wei-Shaw/sub2api` 的拼车场景定制版。核心能力来自上游内置实现，并在此基础上
补充了**种子迁移 + 少量后端防护补丁**（空响应体错误透传、Responses `instructions` 与末尾
`system`/`developer` 关键词拦截），前端代码零改动。

- 定制种子：`backend/migrations/251_carpool_seed.sql`（服务启动时自动执行，幂等）
- 种子与补丁测试：`backend/migrations/251_carpool_seed_test.go`、`backend/internal/service/carpool_keyword_bypass_test.go`、`backend/internal/handler/carpool_passthrough_empty_body_test.go`

## 一、能力对照（你提的需求 → 上游实现）

| 需求 | 上游实现 | 定制 |
|---|---|---|
| 车友看限额/过期时间 | API Key 自带 `quota/quota_used/expires_at` + 5h/1d/7d 滚动窗口限流；用户面板 `/api/v1/keys`、`/usage/dashboard/*`、`/user/api-keys/:id/usage/daily` 全部现成 | 无需改动 |
| 车友看使用日志 | 用户端用量列表 `/api/v1/usage` 现成；错误请求页 `/api/v1/usage/errors` 受 `allow_user_view_error_requests` 开关控制（fail-closed，且已脱敏白名单视图） | 种子设为 `false`：车友看不到上游错误明细 |
| 车主添加渠道（标准 API） | 账号体系同时支持 `oauth`（Codex OAuth 等）与 `apikey`（标准 API 接口）两类，管理后台「账号管理」直接添加 | 无需改动 |
| 上游报错伪装 | `ErrorPassthroughService` 兜底规则：命中后用 `custom_message` 改写文案，`response_code` 改写状态码；上游真实错误只进车主的 ops 运维看板（存储时已脱敏凭据字段） | 种子预置 4 条全平台兜底规则 + handler 支持空响应体命中状态码规则 |
| 违规内容前置过滤、不发上游 | `ContentModerationService`（pre_block + keyword_blocking_mode=keyword_only）：本地关键词同步拦截，命中即 403，**不调用任何外部审核 API、不发上游请求** | 种子预置初始关键词表 + 本地关键词补充覆盖 `instructions` 与末尾 `system`/`developer` 项 |
| 车友违规治理 | 关键词命中写入 `content_moderation_logs`；可选自动封号（`auto_ban_enabled` + 阈值） | 种子默认只拦不封，车主可开 |

## 二、部署

### 方式 A：Docker Compose（推荐）

```bash
# 1. 准备部署目录
mkdir -p sub2api-deploy && cd sub2api-deploy

# 2. 用本仓库的 compose 文件与 .env（或按官方 docker-deploy.sh 生成）
cp /path/to/this-repo/deploy/docker-compose.local.yml docker-compose.yml
cp /path/to/this-repo/deploy/.env.example .env
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

> 注意：251 号种子采用 `ON CONFLICT (key) DO NOTHING` / `WHERE NOT EXISTS` 写入，
> 只会补全缺失项，不会覆盖老实例中管理员已经手工保存过的配置。

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
| carpool-mask-5xx | 500/502/503/504/529 | 502 「上游服务暂时不可用…」 |
| carpool-mask-429 | 429 | 429 「用车需求较高…」（保留 429 让客户端正常退避重试） |
| carpool-mask-auth | 401/402/403 | 502 「上游通道维护中…」（绝不暴露车主账号状态） |
| carpool-mask-400 | 400/404/413/422 | 原状态码 + 「请求被上游拒绝…」（客户端可据此修自己的参数） |

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

定制包含以下新增与微调文件，rebase 上游 `main` 时仅需留意 4 个 Go 源码小改点：

- 新增文件：
  - `CARPOOL.md`
  - `backend/migrations/251_carpool_seed.sql`
  - `backend/migrations/251_carpool_seed_test.go`
  - `backend/internal/service/carpool_keyword_bypass_test.go`
  - `backend/internal/handler/carpool_passthrough_empty_body_test.go`
- 修改文件：
  - `backend/cmd/server/VERSION`
  - `backend/internal/handler/openai_gateway_handler.go`（`applyOpenAIFailoverPassthroughRule` 允许空响应体命中状态码规则）
  - `backend/internal/handler/gateway_handler.go`（failover 耗尽透传允许空响应体）
  - `backend/internal/handler/gemini_v1beta_handler.go`（Gemini failover 耗尽透传允许空响应体）
  - `backend/internal/service/content_moderation_input.go`（`extractContentModerationKeywordText` 补充扫描 Responses `instructions` 与末尾 `system`/`developer` 项）

> 注意：迁移文件有 SHA256 校验。如果未来你手工改了已在线上环境执行过的 251 号种子，
> 会触发 checksum mismatch；正确做法是改关键词走管理后台（settings 表），或新增 252 号迁移。

## 五、已知边界

- 关键词提取与匹配边界：
  - 历史上下文、以及以 `assistant`/`tool`/`function_call_output` 结尾的 Agent 工具循环轮次不会触发关键词拦截（保留上游设计，避免工具循环误伤）。
  - 本地关键词已覆盖最新一轮 `user` 消息、Responses `instructions`（非工具循环时）及紧随 `user` 后的末尾 `system`/`developer` 项；匹配为大小写不敏感的子串匹配（`strings.Contains`），在后台追加短关键词时注意避免误伤正常语义。
- 上游报错伪装边界：
  - 400 类确定性错误默认保留状态码（客户端需要知道参数错在哪）。若想连 400 也完全统一成 502，在后台把 `carpool-mask-400` 的「透传状态码」关掉并填 502 即可。
  - 流式请求中上游**已开始输出后**的中断无法伪装（SSE 已发出 200），种子规则只覆盖响应头之前的失败——这也是上游所有反代的一致行为。
  - 空响应体且带 HTTP 状态码的上游失败现已可命中按状态码匹配的规则；但无 HTTP 状态码的网络超时/连接断开仍不会命中状态码规则。
  - OpenAI 凭据/访问态拒绝、过载降级（`IsOpenAICapacityShed`）、兼容模型不存在 400 以及 `cyber_policy` 硬阻断会在透传规则之前返回；Responses WebSocket 模式则使用内置脱敏关闭文案，不读取 `error_passthrough_rules`。
