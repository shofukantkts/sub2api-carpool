package migrations

import (
	"encoding/json"
	"strings"
	"testing"

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
}

// TestCarpoolSeed_RiskControlEnabledOn 校验总开关与车友错误页开关的种子值。
func TestCarpoolSeed_RiskControlEnabledOn(t *testing.T) {
	require.Equal(t, "true", carpoolSettingValue(t, "risk_control_enabled"))
	require.Equal(t, "false", carpoolSettingValue(t, "allow_user_view_error_requests"))
}

// carpoolRuleRow 是 error_passthrough_rules 的一个种子行（解析结果）。
type carpoolRuleRow struct {
	Name            string
	Priority        int
	ErrorCodes      []int
	MatchMode       string
	PassthroughCode bool
	ResponseCode    *int
	PassthroughBody bool
	CustomMessage   string
	SkipMonitoring  bool
}

// parseCarpoolRuleRows 按种子文件的固定列序解析 INSERT 行。
// 列序与 251_carpool_seed.sql 的 INSERT 列清单一一对应：
// (name, enabled, priority, error_codes, keywords, match_mode,
//  platforms, passthrough_code, response_code, passthrough_body,
//  custom_message, skip_monitoring, description, updated_at)
func parseCarpoolRuleRows(t *testing.T) []carpoolRuleRow {
	t.Helper()
	raw := carpoolSeedSQL(t)
	start := strings.Index(raw, "INSERT INTO error_passthrough_rules")
	require.True(t, start >= 0, "seed must insert error_passthrough_rules")
	block := raw[start:]
	end := strings.Index(block, "ON CONFLICT DO NOTHING;")
	require.True(t, end >= 0, "rules INSERT must end with ON CONFLICT DO NOTHING")
	block = block[:end]

	// 按 VALUES 行拆分：先折叠空白，再按 "以字符串感知的右括号" 切元组。
	// "VALUES (" 的左括号开启第一个元组；每行收尾于 ")，" 或最末 ")"。
	collapsed := strings.Join(strings.Fields(block), " ")
	valIdx := strings.Index(collapsed, "VALUES (")
	require.True(t, valIdx >= 0, "rules INSERT must contain VALUES (")
	tuplesOnly := collapsed[valIdx+len("VALUES ("):]
	var rawTuples []string
	buf, inString := strings.Builder{}, false
	for i := 0; i < len(tuplesOnly); i++ {
		c := tuplesOnly[i]
		if c == '\'' {
			inString = !inString
			buf.WriteByte(c)
			continue
		}
		if !inString && c == ')' {
			// 元组内有嵌套括号（NOW()）。右括号只有在「其后为 ", (" 或结尾」时
			// 才是元组收尾；NOW() 的右括号后面直接跟元组收尾 ")" 或内容。
			after := tuplesOnly[i+1:]
			trimmed := strings.TrimSpace(after)
			isTupleClose := strings.HasPrefix(trimmed, ", (") || trimmed == "" ||
				strings.HasPrefix(trimmed, "ON CONFLICT") ||
				(!strings.HasSuffix(trimmed[:min(1, len(trimmed))], ")") && strings.HasPrefix(after, " )"))
			if !isTupleClose {
				buf.WriteByte(c)
				continue
			}
			rawTuples = append(rawTuples, buf.String())
			buf.Reset()
			if strings.HasPrefix(trimmed, ", (") {
				nextOpen := strings.Index(after, "(")
				i += 1 + nextOpen
				continue
			}
			break
		}
		buf.WriteByte(c)
	}
	var rows []carpoolRuleRow
	for _, tuple := range rawTuples {
		fields, err := splitSQLTuple(tuple)
		require.NoError(t, err, "row: %s", tuple)
		require.Len(t, fields, 14, "expected 14 columns per rule row, got %d for %q", len(fields), tuple)

		rule := carpoolRuleRow{
			Name:          sqlString(t, fields[0]),
			MatchMode:     sqlString(t, fields[5]),
			CustomMessage: sqlString(t, fields[10]),
		}
		require.NoError(t, json.Unmarshal([]byte(jsonbLiteral(t, fields[3])), &rule.ErrorCodes))
		require.Equal(t, "'[]'::jsonb", fields[4], "seed rules never rely on keyword matching")
		rule.Priority = mustAtoi(t, fields[2])

		// 列序（见 251_carpool_seed.sql 的 INSERT 列清单）：
		// 0 name, 1 enabled, 2 priority, 3 error_codes, 4 keywords, 5 match_mode,
		// 6 platforms, 7 passthrough_code, 8 response_code, 9 passthrough_body,
		// 10 custom_message, 11 skip_monitoring, 12 description, 13 updated_at
		switch fields[7] {
		case "TRUE", "true":
			rule.PassthroughCode = true
		}
		switch fields[9] {
		case "TRUE", "true":
			rule.PassthroughBody = true
		}
		switch fields[11] {
		case "TRUE", "true":
			rule.SkipMonitoring = true
		}
		if fields[8] != "NULL" {
			code := mustAtoi(t, fields[8])
			rule.ResponseCode = &code
		}

		rows = append(rows, rule)
	}
	return rows
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
			buf.WriteByte(c)
		case c == ',' && !inString:
			fields = append(fields, strings.TrimSpace(buf.String()))
			buf.Reset()
		default:
			buf.WriteByte(c)
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
		// Validate：name 必填、match_mode 合法、至少一个匹配条件、
		// passthrough_code=false 时 response_code 必填、
		// passthrough_body=false 时 custom_message 必填。
		require.NotEmpty(t, rule.Name)
		require.Equal(t, "any", rule.MatchMode)
		require.NotEmpty(t, rule.ErrorCodes, "rule %s must match by status codes", rule.Name)
		require.NotEmpty(t, strings.TrimSpace(rule.CustomMessage), "rule %s must carry a masking message", rule.Name)
		require.False(t, rule.PassthroughBody, "rule %s must never pass through upstream body", rule.Name)
		require.False(t, rule.SkipMonitoring, "rule %s keeps ops records for the owner", rule.Name)

		if !rule.PassthroughCode {
			require.NotNil(t, rule.ResponseCode, "rule %s rewrites status", rule.Name)
			require.Greater(t, *rule.ResponseCode, 0)
		}
	}

	// 语义断言：凭据类(401/402/403)与 5xx 一律改写为 502，
	// 429/400 类保留状态码但改写文案。
	byName := map[string]carpoolRuleRow{}
	for _, r := range rules {
		byName[r.Name] = r
	}
	mask5xx := byName["carpool-mask-5xx"]
	require.Equal(t, []int{500, 502, 503, 504, 529}, mask5xx.ErrorCodes)
	require.False(t, mask5xx.PassthroughCode)
	require.NotNil(t, mask5xx.ResponseCode)
	require.Equal(t, 502, *mask5xx.ResponseCode)

	maskAuth := byName["carpool-mask-auth"]
	require.Equal(t, []int{401, 402, 403}, maskAuth.ErrorCodes)
	require.NotNil(t, maskAuth.ResponseCode)
	require.Equal(t, 502, *maskAuth.ResponseCode)

	mask429 := byName["carpool-mask-429"]
	require.Equal(t, []int{429}, mask429.ErrorCodes)
	require.True(t, mask429.PassthroughCode, "429 keeps upstream status code")

	mask400 := byName["carpool-mask-400"]
	require.Equal(t, []int{400, 404, 413, 422}, mask400.ErrorCodes)
	require.True(t, mask400.PassthroughCode, "4xx request errors keep upstream status code")
}
