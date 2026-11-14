package migrations

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/stretchr/testify/require"
)

// 拼车种子迁移（251_carpool_seed.sql）合法性校验。
// 这些测试保证种子里的 JSON 配置能被后端现有解析器接受，
// 以及兜底改写规则满足 ErrorPassthroughRule.Validate 的约束，
// 避免部署时启动失败或配置静默失效。

func carpoolSeedSQL(t *testing.T) string {
	t.Helper()
	content, err := FS.ReadFile("251_carpool_seed.sql")
	require.NoError(t, err)
	return string(content)
}

// carpoolSettingValue 抽取 settings INSERT 里指定 key 的 value 列。
// 兼容两种写法：VALUES ('key', 'value', NOW()) 与 VALUES (\n 'key', \n '<value>'...
// （251 号种子的 content_moderation_config 是跨行 JSON）。
func carpoolSettingValue(t *testing.T, key string) string {
	t.Helper()
	raw := carpoolSeedSQL(t)
	keyToken := "'" + key + "',"
	idx := strings.Index(raw, keyToken)
	require.True(t, idx >= 0, "seed SQL must contain setting %q", key)
	// 找到 key 之后的第一个字符串字面量起始。
	rest := raw[idx+len(keyToken):]
	quote := strings.Index(rest, "'")
	require.True(t, quote >= 0, "setting %q must have a string value", key)
	rest = rest[quote+1:]
	// value 结束于未被转义的下一个单引号（本种子的 JSON 不含转义引号）。
	end := strings.Index(rest, "'")
	require.True(t, end >= 0, "setting %q value must be closed", key)
	value := strings.ReplaceAll(rest[:end], "''", "'")
	return value
}

// TestCarpoolSeed_ContentModerationConfig 校验内容审计配置 JSON 与
// ContentModerationConfig 解析器约束一致（结构层面等价检查）。
func TestCarpoolSeed_ContentModerationConfig(t *testing.T) {
	raw := carpoolSettingValue(t, "content_moderation_config")

	var cfg struct {
		Engine              string   `json:"engine"`
		Enabled             bool     `json:"enabled"`
		Mode                string   `json:"mode"`
		KeywordBlockingMode string   `json:"keyword_blocking_mode"`
		SampleRate          int      `json:"sample_rate"`
		AllGroups           bool     `json:"all_groups"`
		GroupIDs            []int64  `json:"group_ids"`
		BlockedKeywords     []string `json:"blocked_keywords"`
		BlockStatus         int      `json:"block_status"`
		BlockMessage        string   `json:"block_message"`
		EmailOnHit          bool     `json:"email_on_hit"`
		AutoBanEnabled      bool     `json:"auto_ban_enabled"`
		BanThreshold        int      `json:"ban_threshold"`
		ModelFilter         struct {
			Type   string   `json:"type"`
			Models []string `json:"models"`
		} `json:"model_filter"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &cfg), "content_moderation_config must be valid JSON")

	// validModerationEngine 只接受 openai / typesafe。
	require.Contains(t, []string{"openai", "typesafe"}, cfg.Engine)
	require.True(t, cfg.Enabled, "seed config must be enabled")
	// pre_block + keyword_only：本地关键词同步拦截，不依赖外部审核 API。
	require.Equal(t, "pre_block", cfg.Mode)
	require.Equal(t, "keyword_only", cfg.KeywordBlockingMode)
	// sample_rate=100 防止关键词命中被采样率跳过（shouldSample）。
	require.Equal(t, 100, cfg.SampleRate)
	require.True(t, cfg.AllGroups, "carpool seed covers all groups")
	require.NotEmpty(t, cfg.BlockedKeywords, "seed ships a non-empty keyword list")
	require.Equal(t, 403, cfg.BlockStatus)
	require.NotEmpty(t, strings.TrimSpace(cfg.BlockMessage))
	require.False(t, cfg.AutoBanEnabled, "carpool seed blocks without auto-ban")
	require.Equal(t, "all", cfg.ModelFilter.Type)

	// 回归校验：避免短子串误伤正常医学、农牧、二次元语境。
	benignSamples := []string{
		"请解释如何抑制毒素分泌",
		"需要控制毒性反应",
		"白羽肉鸡的养殖周期是多少",
		"cp资源是什么意思",
	}
	for _, sample := range benignSamples {
		lowerSample := strings.ToLower(sample)
		for _, kw := range cfg.BlockedKeywords {
			require.NotContains(t, lowerSample, strings.ToLower(kw),
				"benign phrase %q must not match keyword %q", sample, kw)
		}
	}
}

// TestCarpoolSeed_RiskControlEnabledOn 校验总开关与车友错误页开关的种子值。
func TestCarpoolSeed_RiskControlEnabledOn(t *testing.T) {
	require.Equal(t, "true", carpoolSettingValue(t, "risk_control_enabled"))
	require.Equal(t, "false", carpoolSettingValue(t, "allow_user_view_error_requests"))
}

// parseCarpoolRuleRows 按种子文件的固定列序解析 INSERT ... FROM (VALUES ...) 行，
// 并直接构造 model.ErrorPassthroughRule，供 Validate() 校验。
// 列序与 251_carpool_seed.sql 的 VALUES 列清单一一对应：
// (name, enabled, priority, error_codes, keywords, match_mode,
// platforms, passthrough_code, response_code, passthrough_body,
// custom_message, skip_monitoring, description)
func parseCarpoolRuleRows(t *testing.T) []*model.ErrorPassthroughRule {
	t.Helper()
	raw := carpoolSeedSQL(t)
	start := strings.Index(raw, "INSERT INTO error_passthrough_rules")
	require.True(t, start >= 0, "seed must insert error_passthrough_rules")
	block := raw[start:]
	// error_passthrough_rules 在 name 上无唯一约束，必须用 WHERE NOT EXISTS 保证幂等。
	require.NotContains(t, block, "ON CONFLICT", "error_passthrough_rules must not rely on ON CONFLICT without a unique constraint")
	require.Contains(t, block, "WHERE NOT EXISTS", "error_passthrough_rules must deduplicate via WHERE NOT EXISTS")

	valStart := strings.Index(block, "VALUES")
	require.True(t, valStart >= 0, "rules INSERT must contain VALUES")
	asVEnd := strings.Index(block[valStart:], ") AS v")
	require.True(t, asVEnd >= 0, "rules VALUES block must close with ) AS v")
	tuplesOnly := strings.TrimSpace(block[valStart+len("VALUES") : valStart+asVEnd])

	var rawTuples []string
	var buf strings.Builder
	inString := false
	depth := 0
	for i := 0; i < len(tuplesOnly); i++ {
		c := tuplesOnly[i]
		if c == '\'' {
			_ = buf.WriteByte(c)
			// 处理 SQL 单引号转义 ''
			if inString && i+1 < len(tuplesOnly) && tuplesOnly[i+1] == '\'' {
				_ = buf.WriteByte(tuplesOnly[i+1])
				i++
				continue
			}
			inString = !inString
			continue
		}
		if !inString {
			switch c {
			case '(':
				if depth == 0 {
					buf.Reset()
					depth = 1
					continue
				}
				depth++
			case ')':
				depth--
				if depth == 0 {
					rawTuples = append(rawTuples, buf.String())
					buf.Reset()
					continue
				}
			}
		}
		if depth > 0 {
			_ = buf.WriteByte(c)
		}
	}
	require.Equal(t, 0, depth, "unbalanced parentheses in VALUES block")

	var rows []*model.ErrorPassthroughRule
	for _, tuple := range rawTuples {
		fields, err := splitSQLTuple(tuple)
		require.NoError(t, err, "row: %s", tuple)
		require.Len(t, fields, 13, "expected 13 columns per VALUES row, got %d for %q", len(fields), tuple)

		customMsg := sqlString(t, fields[10])
		desc := sqlString(t, fields[12])
		rule := &model.ErrorPassthroughRule{
			Name:          sqlString(t, fields[0]),
			Enabled:       parseSQLBool(t, fields[1]),
			Priority:      mustAtoi(t, fields[2]),
			MatchMode:     sqlString(t, fields[5]),
			CustomMessage: &customMsg,
			Description:   &desc,
		}
		require.NoError(t, json.Unmarshal([]byte(jsonbLiteral(t, fields[3])), &rule.ErrorCodes))
		require.NoError(t, json.Unmarshal([]byte(jsonbLiteral(t, fields[4])), &rule.Keywords))
		require.Empty(t, rule.Keywords, "seed rules never rely on keyword matching")
		require.NoError(t, json.Unmarshal([]byte(jsonbLiteral(t, fields[6])), &rule.Platforms))
		rule.PassthroughCode = parseSQLBool(t, fields[7])
		rule.PassthroughBody = parseSQLBool(t, fields[9])
		rule.SkipMonitoring = parseSQLBool(t, fields[11])

		if codeField := strings.TrimSuffix(fields[8], "::integer"); codeField != "NULL" {
			code := mustAtoi(t, codeField)
			rule.ResponseCode = &code
		}

		rows = append(rows, rule)
	}
	return rows
}

func parseSQLBool(t *testing.T, s string) bool {
	t.Helper()
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "TRUE":
		return true
	case "FALSE":
		return false
	default:
		t.Fatalf("not a SQL boolean literal: %q", s)
		return false
	}
}

// splitSQLTuple 按顶层逗号切分一个 SQL 元组内容（括号已剥离）。
// 字符串字面量（'...'）内部的逗号不参与切分；本种子文件的字符串不含引号转义。
func splitSQLTuple(s string) ([]string, error) {
	var fields []string
	var buf strings.Builder
	inString := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			inString = !inString
			_ = buf.WriteByte(c)
		case c == ',' && !inString:
			fields = append(fields, strings.TrimSpace(buf.String()))
			buf.Reset()
		default:
			_ = buf.WriteByte(c)
		}
	}
	fields = append(fields, strings.TrimSpace(buf.String()))
	return fields, nil
}

// sqlString 去掉 SQL 字符串字面量两侧的单引号。
func sqlString(t *testing.T, s string) string {
	t.Helper()
	require.True(t, strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'"), "not a SQL string literal: %q", s)
	inner := s[1 : len(s)-1]
	return strings.ReplaceAll(inner, "''", "'")
}

// jsonbLiteral 解析 "'<json>'::jsonb" 形式的 JSONB 字面量，返回其中的 JSON 文本。
func jsonbLiteral(t *testing.T, s string) string {
	t.Helper()
	require.True(t, strings.HasSuffix(s, "'::jsonb"), "not a jsonb literal: %q", s)
	return strings.TrimSuffix(strings.TrimPrefix(s, "'"), "'::jsonb")
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			t.Fatalf("not a non-negative integer: %q", s)
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// TestCarpoolSeed_ErrorPassthroughRules 逐条校验伪装规则满足
// model.ErrorPassthroughRule.Validate 的约束，并符合车友伪装语义。
func TestCarpoolSeed_ErrorPassthroughRules(t *testing.T) {
	rules := parseCarpoolRuleRows(t)
	require.Len(t, rules, 4, "seed ships exactly 4 masking rules")

	for _, rule := range rules {
		require.NoError(t, rule.Validate(), "rule %s must pass model.ErrorPassthroughRule.Validate", rule.Name)
		require.True(t, rule.Enabled, "rule %s must be enabled", rule.Name)
		require.Equal(t, model.MatchModeAny, rule.MatchMode)
		require.Empty(t, rule.Platforms, "rule %s applies to all platforms", rule.Name)
		require.NotEmpty(t, rule.ErrorCodes, "rule %s must match by status codes", rule.Name)
		require.NotNil(t, rule.CustomMessage)
		require.NotEmpty(t, strings.TrimSpace(*rule.CustomMessage), "rule %s must carry a masking message", rule.Name)
		require.False(t, rule.PassthroughBody, "rule %s must never pass through upstream body", rule.Name)
		require.False(t, rule.SkipMonitoring, "rule %s keeps ops records for the owner", rule.Name)
	}

	// 语义断言：凭据类(401/402/403)与 5xx 一律改写为 502，
	// 429/400 类保留状态码但改写文案。
	byName := map[string]*model.ErrorPassthroughRule{}
	for _, r := range rules {
		byName[r.Name] = r
	}
	mask5xx := byName["carpool-mask-5xx"]
	require.NotNil(t, mask5xx)
	require.Equal(t, []int{500, 502, 503, 504, 529}, mask5xx.ErrorCodes)
	require.False(t, mask5xx.PassthroughCode)
	require.NotNil(t, mask5xx.ResponseCode)
	require.Equal(t, 502, *mask5xx.ResponseCode)

	maskAuth := byName["carpool-mask-auth"]
	require.NotNil(t, maskAuth)
	require.Equal(t, []int{401, 402, 403}, maskAuth.ErrorCodes)
	require.False(t, maskAuth.PassthroughCode)
	require.NotNil(t, maskAuth.ResponseCode)
	require.Equal(t, 502, *maskAuth.ResponseCode)

	mask429 := byName["carpool-mask-429"]
	require.NotNil(t, mask429)
	require.Equal(t, []int{429}, mask429.ErrorCodes)
	require.True(t, mask429.PassthroughCode, "429 keeps upstream status code")

	mask400 := byName["carpool-mask-400"]
	require.NotNil(t, mask400)
	require.Equal(t, []int{400, 404, 413, 422}, mask400.ErrorCodes)
	require.True(t, mask400.PassthroughCode, "4xx request errors keep upstream status code")
}
