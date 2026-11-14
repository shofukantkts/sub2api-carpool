-- 251_carpool_seed.sql
-- 拼车（车主/车友）场景预置配置：内容审计关键词拦截 + 上游报错伪装。
--
-- 前提说明：
--   1. 拦截与伪装能力全部来自上游已有的实现，本迁移只写 settings 行与
--      error_passthrough_rules 种子，不改任何代码路径。
--   2. 内容审计关键词走「本地关键词、同步拦截」：模式 pre_block +
--      keyword_blocking_mode=keyword_only，请求在进入上游模型前就被
--      ContentModerationService.Check 命中并返回 403，不产生任何上游调用，
--      也不依赖任何外部审核 API（无需配置 api_keys）。
--   3. 上游报错伪装走 ErrorPassthroughService 兜底规则：所有平台命中后，
--      4xx/5xx 全部统一改写为 502 与同一条通用文案，不区分错误类型；
--      真实上游错误只留在车主的管理端运维看板里。
--   4. 全部语句幂等（settings 使用 ON CONFLICT DO NOTHING；error_passthrough_rules
--      在 name 上无唯一约束，故使用 WHERE NOT EXISTS 按 name 防重），可在已有
--      实例上重复执行且不覆盖管理员已有的手工配置。

-- ---------------------------------------------------------------------------
-- 1. 风控中心总开关（车友场景默认开启）
-- ---------------------------------------------------------------------------
-- risk_control_enabled 同时控制：
--   - ContentModerationService.Check 是否生效（runtimeSnapshot.riskControlEnabled）
--   - 提示词审计（prompt audit）的门控
--   - 前端「风控中心」菜单的可见性（featureFlags.riskControl, opt-in）
INSERT INTO settings (key, value, updated_at)
VALUES ('risk_control_enabled', 'true', NOW())
ON CONFLICT (key) DO NOTHING;

-- 车友可见错误请求页：关（默认即 false，显式写入以保证口径一致）。
-- 车友仍能看到自己的限额/到期/用量统计；上游真实错误体不会出现在车友端。
INSERT INTO settings (key, value, updated_at)
VALUES ('allow_user_view_error_requests', 'false', NOW())
ON CONFLICT (key) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 2. 内容审计配置（本地关键词同步拦截，无外部审核 API）
-- ---------------------------------------------------------------------------
-- JSON 结构与 ContentModerationConfig（backend/internal/service/content_moderation.go）
-- 对齐；parseContentModerationConfig 会做 normalize，缺省字段走默认值。
-- 关键说明：
--   - mode=pre_block：同步前置拦截。
--   - keyword_blocking_mode=keyword_only：只跑本地关键词匹配，
--     不调用任何外部审核模型（skip_api_keyword_only 提前返回），
--     因此 api_keys 留空是合法且预期状态。
--   - blocked_keywords 为初始关键词集，车主可在管理后台
--     「风控中心 → 内容审计」里增删改，保存后立即生效（1s 内热加载）。
--   - block_status/block_message：车友看到的拦截响应。
--   - auto_ban_enabled=false：只拦不封号，避免误伤付费车友；车主可自行开启。
INSERT INTO settings (key, value, updated_at)
VALUES (
    'content_moderation_config',
    '{
  "engine": "openai",
  "enabled": true,
  "mode": "pre_block",
  "keyword_blocking_mode": "keyword_only",
  "sample_rate": 100,
  "all_groups": true,
  "group_ids": [],
  "record_non_hits": false,
  "pre_hash_check_enabled": false,
  "blocked_keywords": [
    "child sexual", "csam", "儿童色情", "幼女", "未成年性行为",
    "制作炸弹", "制毒教程", "毒品制造", "合成毒品", "冰毒制作", "芬太尼合成",
    "生物武器", "基因编辑病毒", "病原体改造", "细菌战", "炭疽培养",
    "渗透入侵教程", "拿站教程", "ddos攻击", "抓肉鸡", "僵尸网络", "爆破跑库",
    "勒索病毒部署", "盗号木马编写", "银行卡四件套", "洗钱通道",
    "枪支买卖", "弹药购买", "人体器官交易", "人口贩卖",
    "自杀教程", "自残方法",
    "翻墙教程变现", "色情平台搭建", "淫秽网站架设"
  ],
  "block_status": 403,
  "block_message": "您的请求命中了本服务的内容合规规则，已被拒绝执行。如有疑问请联系车主。",
  "email_on_hit": false,
  "auto_ban_enabled": false,
  "ban_threshold": 10,
  "violation_window_hours": 720,
  "hit_retention_days": 180,
  "non_hit_retention_days": 3,
  "model_filter": { "type": "all", "models": [] }
}',
    NOW()
)
ON CONFLICT (key) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 3. 上游报错伪装（兜底改写规则）
-- ---------------------------------------------------------------------------
-- ErrorPassthroughRule 表（backend/internal/model/error_passthrough_rule.go）：
--   - platforms='[]'  → 匹配所有平台（openai/anthropic/gemini/antigravity/...）
--   - match_mode=any + error_codes  → 仅按状态码匹配
--   - passthrough_code=false + response_code=502 → 所有命中的上游错误统一改写为 502
--   - passthrough_body=false + custom_message → 全部使用同一条通用文案，永不透传上游真实报错
--   - skip_monitoring=false → 车主运维看板仍记录真实上游错误（含脱敏 body），
--     车友端永远只看到同一套 502 与通用提示。
-- 优先级数字越小越先匹配；保留按类别拆分规则，方便车主后续按需在后台单独放开。
-- 注意：error_passthrough_rules 表在 name 上没有 UNIQUE 约束，不能依赖
-- ON CONFLICT DO NOTHING 防重，必须用 WHERE NOT EXISTS 保证幂等。
INSERT INTO error_passthrough_rules (
    name, enabled, priority, error_codes, keywords, match_mode,
    platforms, passthrough_code, response_code, passthrough_body,
    custom_message, skip_monitoring, description, updated_at
)
SELECT
    v.name, v.enabled, v.priority, v.error_codes, v.keywords, v.match_mode,
    v.platforms, v.passthrough_code, v.response_code, v.passthrough_body,
    v.custom_message, v.skip_monitoring, v.description, NOW()
FROM (
    VALUES
        (
            'carpool-mask-5xx',
            TRUE, 10,
            '[500,502,503,504,529]'::jsonb, '[]'::jsonb, 'any',
            '[]'::jsonb,
            FALSE, 502, FALSE,
            '上游服务暂时不可用，请稍后重试；若持续失败请联系车主。',
            FALSE,
            '上游 5xx/过载 → 统一 502 通用文案'
        ),
        (
            'carpool-mask-429',
            TRUE, 10,
            '[429]'::jsonb, '[]'::jsonb, 'any',
            '[]'::jsonb,
            FALSE, 502, FALSE,
            '上游服务暂时不可用，请稍后重试；若持续失败请联系车主。',
            FALSE,
            '上游 429 → 统一 502 通用文案'
        ),
        (
            'carpool-mask-auth',
            TRUE, 10,
            '[401,402,403]'::jsonb, '[]'::jsonb, 'any',
            '[]'::jsonb,
            FALSE, 502, FALSE,
            '上游服务暂时不可用，请稍后重试；若持续失败请联系车主。',
            FALSE,
            '上游凭据/账单类错误 → 统一 502 通用文案'
        ),
        (
            'carpool-mask-400',
            TRUE, 20,
            '[400,404,413,422]'::jsonb, '[]'::jsonb, 'any',
            '[]'::jsonb,
            FALSE, 502, FALSE,
            '上游服务暂时不可用，请稍后重试；若持续失败请联系车主。',
            FALSE,
            '确定性请求错误 → 统一 502 通用文案'
        )
) AS v (
    name, enabled, priority, error_codes, keywords, match_mode,
    platforms, passthrough_code, response_code, passthrough_body,
    custom_message, skip_monitoring, description
)
WHERE NOT EXISTS (
    SELECT 1 FROM error_passthrough_rules existing WHERE existing.name = v.name
);
