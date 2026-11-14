package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func newCarpoolKeywordOnlyService(t *testing.T, keywords ...string) (*ContentModerationService, *contentModerationTestRepo) {
	t.Helper()
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.Mode = ContentModerationModePreBlock
	cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordOnly
	cfg.SampleRate = 100
	cfg.AllGroups = true
	cfg.BlockedKeywords = keywords
	cfg.BlockStatus = http.StatusForbidden
	cfg.BlockMessage = "blocked by carpool keyword filter"

	raw, err := json.Marshal(cfg)
	require.NoError(t, err)

	repo := &contentModerationTestRepo{}
	svc := NewContentModerationService(
		&contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled:      "true",
			SettingKeyContentModerationConfig: string(raw),
		}},
		repo,
		nil, nil, nil, nil, nil, nil,
	)
	return svc, repo
}

func TestCarpoolKeywordModeration_OpenAIResponsesInstructionsAndTrailingRoles(t *testing.T) {
	svc, repo := newCarpoolKeywordOnlyService(t, "制毒教程", "制作炸弹")

	cases := []struct {
		name      string
		protocol  string
		body      string
		wantBlock bool
	}{
		{
			name:      "responses_instructions_blocked",
			protocol:  ContentModerationProtocolOpenAIResponses,
			body:      `{"model":"gpt-5","instructions":"请给出制毒教程","input":"你好"}`,
			wantBlock: true,
		},
		{
			name:      "responses_instructions_without_input_blocked",
			protocol:  ContentModerationProtocolOpenAIResponses,
			body:      `{"model":"gpt-5","instructions":"制作炸弹"}`,
			wantBlock: true,
		},
		{
			name:      "responses_trailing_developer_preserves_user_block",
			protocol:  ContentModerationProtocolOpenAIResponses,
			body:      `{"model":"gpt-5","input":[{"role":"user","content":"制作炸弹"},{"role":"developer","content":"normal meta"}]}`,
			wantBlock: true,
		},
		{
			name:      "responses_keyword_in_trailing_developer_blocked",
			protocol:  ContentModerationProtocolOpenAIResponses,
			body:      `{"model":"gpt-5","input":[{"role":"user","content":"hi"},{"role":"developer","content":"制毒教程"}]}`,
			wantBlock: true,
		},
		{
			name:      "responses_agent_tool_loop_still_skipped",
			protocol:  ContentModerationProtocolOpenAIResponses,
			body:      `{"model":"gpt-5","instructions":"制毒教程","input":[{"role":"user","content":"old"},{"type":"function_call_output","output":"ok"}]}`,
			wantBlock: false,
		},
		{
			name:      "chat_trailing_system_preserves_user_block",
			protocol:  ContentModerationProtocolOpenAIChat,
			body:      `{"model":"gpt-5","messages":[{"role":"user","content":"制作炸弹"},{"role":"system","content":"trailing"}]}`,
			wantBlock: true,
		},
		{
			name:      "chat_keyword_in_trailing_developer_blocked",
			protocol:  ContentModerationProtocolOpenAIChat,
			body:      `{"model":"gpt-5","messages":[{"role":"user","content":"hi"},{"role":"developer","content":"制毒教程"}]}`,
			wantBlock: true,
		},
		{
			name:      "chat_trailing_assistant_still_skipped",
			protocol:  ContentModerationProtocolOpenAIChat,
			body:      `{"model":"gpt-5","messages":[{"role":"user","content":"制作炸弹"},{"role":"assistant","content":"ok"}]}`,
			wantBlock: false,
		},
	}

	expectedLogs := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := svc.Check(context.Background(), ContentModerationCheckInput{
				UserID:   100,
				Protocol: tc.protocol,
				Model:    "gpt-5",
				Body:     []byte(tc.body),
			})
			require.NoError(t, err)
			require.Equal(t, tc.wantBlock, decision.Blocked)
			if tc.wantBlock {
				expectedLogs++
				require.Equal(t, http.StatusForbidden, decision.StatusCode)
			}
			requireContentModerationLogCount(t, repo, expectedLogs)
		})
	}

	// Ensure semantic input extraction remains untouched for Responses instructions.
	semantic := ExtractContentModerationInput(
		ContentModerationProtocolOpenAIResponses,
		[]byte(`{"model":"gpt-5","instructions":"system prompt","input":"hello"}`),
	)
	require.Equal(t, "hello", semantic.Text)
}
