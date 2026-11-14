package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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

func newCarpoolMaskStatusRule(status int, customMessage string) *model.ErrorPassthroughRule {
	responseCode := http.StatusBadGateway
	return &model.ErrorPassthroughRule{
		ID:              2,
		Name:            "carpool-mask-4xx",
		Enabled:         true,
		Priority:        10,
		ErrorCodes:      []int{status},
		MatchMode:       model.MatchModeAny,
		Platforms:       []string{},
		PassthroughCode: false,
		ResponseCode:    &responseCode,
		PassthroughBody: false,
		CustomMessage:   &customMessage,
	}
}

// carpoolHTTPUpstreamStub returns a fresh response body for each request. The
// forwarding paths under test use Do (not DoWithTLS); embedding the interface
// keeps this focused stub compatible with service.HTTPUpstream.
type carpoolHTTPUpstreamStub struct {
	service.HTTPUpstream
	statusCode int
	body       []byte
}

func (u *carpoolHTTPUpstreamStub) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return &http.Response{
		StatusCode: u.statusCode,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(u.body)),
	}, nil
}

func newCarpoolOpenAIForwardService(upstream service.HTTPUpstream) *service.OpenAIGatewayService {
	cfg := &config.Config{}
	return service.NewOpenAIGatewayService(
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		service.NewBillingService(cfg, nil),
		nil,
		nil,
		upstream,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
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

	t.Run("gateway_responses_credential_failure_is_overridden", func(t *testing.T) {
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
		require.Contains(t, rec.Body.String(), "上游服务暂时不可用，请稍后重试")
		require.NotContains(t, rec.Body.String(), service.AntigravityCredentialRejectedClientMessage)
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

func TestCarpoolNonFailoverEmbeddingsErrorUsesPassthroughRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(nil))

	upstream := &carpoolHTTPUpstreamStub{statusCode: http.StatusUnprocessableEntity}
	h := newCarpoolOpenAIForwardService(upstream)
	ruleSvc := service.NewErrorPassthroughService(&errorPassthroughRepoStub{
		rules: []*model.ErrorPassthroughRule{
			newCarpoolMaskStatusRule(http.StatusUnprocessableEntity, "embeddings request rejected"),
		},
	}, nil)
	service.BindErrorPassthroughService(c, ruleSvc)

	account := &service.Account{
		ID:       301,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat.example",
		},
	}
	result, err := h.ForwardEmbeddings(
		context.Background(),
		c,
		account,
		[]byte(`{"model":"text-embedding-3-small","input":"hello"}`),
		"",
	)

	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "embeddings request rejected")
	require.NotContains(t, rec.Body.String(), "Unprocessable Entity")
}

func TestCarpoolNonFailoverAlphaSearchErrorUsesPassthroughRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", bytes.NewReader(nil))

	upstream := &carpoolHTTPUpstreamStub{statusCode: http.StatusUnprocessableEntity}
	h := newCarpoolOpenAIForwardService(upstream)
	ruleSvc := service.NewErrorPassthroughService(&errorPassthroughRepoStub{
		rules: []*model.ErrorPassthroughRule{
			newCarpoolMaskStatusRule(http.StatusUnprocessableEntity, "alpha search request rejected"),
		},
	}, nil)
	service.BindErrorPassthroughService(c, ruleSvc)

	account := &service.Account{
		ID:       302,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat.example",
		},
	}
	result, err := h.ForwardAlphaSearch(
		context.Background(),
		c,
		account,
		[]byte(`{"model":"gpt-5.6-sol","commands":{}}`),
	)

	require.NoError(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "alpha search request rejected")
	require.NotContains(t, rec.Body.String(), "Unprocessable Entity")
}

func TestCarpoolNonFailoverAlphaSearchResponsesFallbackUsesPassthroughRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", bytes.NewReader(nil))

	upstream := &carpoolHTTPUpstreamStub{statusCode: http.StatusUnprocessableEntity}
	h := newCarpoolOpenAIForwardService(upstream)
	ruleSvc := service.NewErrorPassthroughService(&errorPassthroughRepoStub{
		rules: []*model.ErrorPassthroughRule{
			newCarpoolMaskStatusRule(http.StatusUnprocessableEntity, "responses search request rejected"),
		},
	}, nil)
	service.BindErrorPassthroughService(c, ruleSvc)

	account := &service.Account{
		ID:          303,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "at-test-token",
			"auth_mode":          service.OpenAIAuthModePersonalAccessToken,
			"chatgpt_account_id": "chatgpt-account",
		},
	}
	result, err := h.ForwardAlphaSearch(
		context.Background(),
		c,
		account,
		[]byte(`{"model":"gpt-5.6-sol","commands":{"search_query":[{"q":"news"}]}}`),
	)

	require.NoError(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "responses search request rejected")
	require.NotContains(t, rec.Body.String(), "Unprocessable Entity")
}

func TestCarpoolNonFailoverOpenAINativeErrorsWithoutMatchingRuleAreMasked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "provider-only diagnostic: account=secret"

	t.Run("embeddings unknown status", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(nil))

		h := newCarpoolOpenAIForwardService(&carpoolHTTPUpstreamStub{
			statusCode: http.StatusTeapot,
			body:       []byte(`{"error":{"message":"` + secret + `"}}`),
		})
		account := &service.Account{
			ID:       304,
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"api_key":  "sk-test",
				"base_url": "https://compat.example",
			},
		}

		result, err := h.ForwardEmbeddings(context.Background(), c, account, []byte(`{"model":"text-embedding-3-small","input":"hello"}`), "")
		require.Error(t, err)
		require.Nil(t, result)
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Equal(t, service.CarpoolMaskedUpstreamClientMessage, gjson.Get(rec.Body.String(), "error.message").String())
		require.NotContains(t, rec.Body.String(), secret)
	})

	t.Run("alpha search unknown status", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", bytes.NewReader(nil))

		h := newCarpoolOpenAIForwardService(&carpoolHTTPUpstreamStub{
			statusCode: http.StatusTeapot,
			body:       []byte(`{"error":{"message":"` + secret + `"}}`),
		})
		account := &service.Account{
			ID:       305,
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"api_key":  "sk-test",
				"base_url": "https://compat.example",
			},
		}

		result, err := h.ForwardAlphaSearch(context.Background(), c, account, []byte(`{"model":"gpt-5.6-sol","commands":{"search_query":[{"q":"news"}]}}`))
		require.NoError(t, err)
		require.Nil(t, result)
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Equal(t, service.CarpoolMaskedUpstreamClientMessage, gjson.Get(rec.Body.String(), "error.message").String())
		require.NotContains(t, rec.Body.String(), secret)
	})
}

func TestCarpoolAnthropicOpenAIAccountFailoverUsesPassthroughRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(nil))

	h := &OpenAIGatewayHandler{
		errorPassthroughService: service.NewErrorPassthroughService(&errorPassthroughRepoStub{
			rules: []*model.ErrorPassthroughRule{newCarpoolMask502Rule()},
		}, nil),
	}
	h.handleAnthropicFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:   http.StatusBadGateway,
		ResponseBody: []byte(`{"error":{"message":"secret upstream detail"}}`),
	}, false)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.Equal(t, "上游服务暂时不可用，请稍后重试", gjson.Get(rec.Body.String(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "secret upstream detail")
}

func TestCarpoolCredentialFailoverUsesAuthPassthroughBeforeInternalMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rule := newCarpoolMaskStatusRule(http.StatusUnauthorized, "上游服务暂时不可用，请稍后重试；若持续失败请联系管理员。")
	rule.Name = "carpool-mask-auth"

	t.Run("chat completions", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))
		h := &GatewayHandler{errorPassthroughService: service.NewErrorPassthroughService(&errorPassthroughRepoStub{rules: []*model.ErrorPassthroughRule{rule}}, nil)}
		h.handleCCFailoverExhaustedWithPlatform(c, &service.UpstreamFailoverError{
			StatusCode: http.StatusUnauthorized,
			Stage:      service.GatewayFailureStageAccountAuth,
			Scope:      service.GatewayFailureScopeAccount,
			Reason:     service.AntigravityCredentialRejectedReason,
		}, service.PlatformOpenAI, false)

		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Contains(t, rec.Body.String(), "上游服务暂时不可用，请稍后重试；若持续失败请联系管理员。")
		require.NotContains(t, rec.Body.String(), service.AntigravityCredentialRejectedClientMessage)
	})

	t.Run("native openai responses", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
		h := &OpenAIGatewayHandler{errorPassthroughService: service.NewErrorPassthroughService(&errorPassthroughRepoStub{rules: []*model.ErrorPassthroughRule{rule}}, nil)}
		h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
			StatusCode: http.StatusUnauthorized,
			Stage:      service.GatewayFailureStageAccountAuth,
			Scope:      service.GatewayFailureScopeAccount,
			Reason:     service.AntigravityCredentialRejectedReason,
		}, false)

		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Contains(t, rec.Body.String(), "上游服务暂时不可用，请稍后重试；若持续失败请联系管理员。")
		require.NotContains(t, rec.Body.String(), service.AntigravityCredentialRejectedClientMessage)
	})
}

func TestCarpoolChatFailoverAfterHeartbeatWritesTerminalError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))
	written, err := rec.WriteString(": keepalive\n\n")
	require.NoError(t, err)
	recordGatewayStreamHeartbeat(c, written)

	(&GatewayHandler{}).handleCCFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusBadGateway,
	}, true)

	body := rec.Body.String()
	require.Contains(t, body, ": keepalive\n\n")
	require.Contains(t, body, "data: {\"error\":")
	require.Contains(t, body, `"type":"server_error"`)
	require.Contains(t, body, "All available accounts exhausted")
	require.Contains(t, body, "data: [DONE]")
	streamErr, ok := service.GetOpsStreamError(c)
	require.True(t, ok)
	require.Equal(t, "server_error", streamErr.ErrType)
	require.Equal(t, http.StatusBadGateway, streamErr.IntendedStatus)
}
