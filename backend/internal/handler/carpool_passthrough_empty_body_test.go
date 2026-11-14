package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newCarpoolMask502Rule(keywords ...string) *model.ErrorPassthroughRule {
	respCode := http.StatusBadGateway
	msg := "上游服务暂时不可用，请稍后重试"
	return &model.ErrorPassthroughRule{
		ID:              1,
		Name:            "carpool-mask-5xx",
		Enabled:         true,
		Priority:        10,
		ErrorCodes:      []int{http.StatusBadGateway},
		Keywords:        keywords,
		MatchMode:       model.MatchModeAny,
		Platforms:       []string{},
		PassthroughCode: false,
		ResponseCode:    &respCode,
		PassthroughBody: false,
		CustomMessage:   &msg,
	}
}

func TestCarpoolFailoverExhausted_EmptyBodyMatchesStatusRule(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("openai", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

		h := newBodyLimitPassthroughHandler(t, newCarpoolMask502Rule())
		h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: nil,
		}, false)

		require.Equal(t, http.StatusBadGateway, rec.Code)
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
		errObj, ok := envelope["error"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "upstream_error", errObj["type"])
		require.Equal(t, "上游服务暂时不可用，请稍后重试", errObj["message"])
	})

	t.Run("openai_keyword_all_rule_does_not_match_empty_body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

		rule := newCarpoolMask502Rule("specific-keyword")
		rule.MatchMode = model.MatchModeAll
		h := newBodyLimitPassthroughHandler(t, rule)
		h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: nil,
		}, false)

		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Contains(t, rec.Body.String(), "Upstream service temporarily unavailable")
	})

	t.Run("gateway_anthropic", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(nil))

		repo := &errorPassthroughRepoStub{rules: []*model.ErrorPassthroughRule{newCarpoolMask502Rule()}}
		h := &GatewayHandler{
			errorPassthroughService: service.NewErrorPassthroughService(repo, nil),
		}
		h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: nil,
		}, service.PlatformAnthropic, false)

		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Contains(t, rec.Body.String(), "上游服务暂时不可用，请稍后重试")
	})

	t.Run("gemini", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", bytes.NewReader(nil))

		repo := &errorPassthroughRepoStub{rules: []*model.ErrorPassthroughRule{newCarpoolMask502Rule()}}
		h := &GatewayHandler{
			errorPassthroughService: service.NewErrorPassthroughService(repo, nil),
		}
		h.handleGeminiFailoverExhausted(c, &service.UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: nil,
		})

		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Contains(t, rec.Body.String(), "上游服务暂时不可用，请稍后重试")
	})

	t.Run("gateway_responses_failover_exhausted_matches_rule", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

		repo := &errorPassthroughRepoStub{rules: []*model.ErrorPassthroughRule{newCarpoolMask502Rule()}}
		h := &GatewayHandler{
			errorPassthroughService: service.NewErrorPassthroughService(repo, nil),
		}
		h.handleResponsesFailoverExhausted(c, &service.UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: nil,
		}, false)

		require.Equal(t, http.StatusBadGateway, rec.Code)
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
		errObj, ok := envelope["error"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "upstream_error", errObj["code"])
		require.Equal(t, "上游服务暂时不可用，请稍后重试", errObj["message"])
	})

	t.Run("gateway_responses_failover_exhausted_sse_matches_rule", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
		written, err := rec.WriteString(": keepalive\n\n")
		require.NoError(t, err)
		recordGatewayStreamHeartbeat(c, written)

		repo := &errorPassthroughRepoStub{rules: []*model.ErrorPassthroughRule{newCarpoolMask502Rule()}}
		h := &GatewayHandler{
			errorPassthroughService: service.NewErrorPassthroughService(repo, nil),
		}
		h.handleResponsesFailoverExhausted(c, &service.UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: nil,
		}, true)

		require.Contains(t, rec.Body.String(), `"type":"response.failed"`)
		require.Contains(t, rec.Body.String(), "上游服务暂时不可用，请稍后重试")
	})

	t.Run("gateway_responses_credential_failure_not_overridden", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

		repo := &errorPassthroughRepoStub{rules: []*model.ErrorPassthroughRule{newCarpoolMask502Rule()}}
		h := &GatewayHandler{
			errorPassthroughService: service.NewErrorPassthroughService(repo, nil),
		}
		h.handleResponsesFailoverExhausted(c, &service.UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: []byte("invalid token"),
			Stage:        service.GatewayFailureStageAccountAuth,
			Reason:       service.AntigravityCredentialRejectedReason,
		}, false)

		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Contains(t, rec.Body.String(), service.AntigravityCredentialRejectedClientMessage)
	})

	t.Run("gateway_cc_failover_exhausted_matches_rule", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))

		repo := &errorPassthroughRepoStub{rules: []*model.ErrorPassthroughRule{newCarpoolMask502Rule()}}
		h := &GatewayHandler{
			errorPassthroughService: service.NewErrorPassthroughService(repo, nil),
		}
		h.handleCCFailoverExhausted(c, &service.UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: nil,
		}, false)

		require.Equal(t, http.StatusBadGateway, rec.Code)
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
		errObj, ok := envelope["error"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "upstream_error", errObj["type"])
		require.Equal(t, "上游服务暂时不可用，请稍后重试", errObj["message"])
	})
}
