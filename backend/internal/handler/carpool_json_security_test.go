package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCarpoolSecurityAuditRejectsDuplicateJSONKeysAcrossProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name     string
		protocol string
		body     string
	}{
		{name: "openai chat top level", protocol: service.ContentModerationProtocolOpenAIChat, body: `{"input":"safe","input":"unsafe"}`},
		{name: "openai responses nested", protocol: service.ContentModerationProtocolOpenAIResponses, body: `{"input":[{"content":{"text":"safe","text":"unsafe"}}]}`},
		{name: "anthropic nested", protocol: service.ContentModerationProtocolAnthropicMessages, body: `{"messages":[{"content":{"text":"safe","text":"unsafe"}}]}`},
		{name: "gemini nested", protocol: service.ContentModerationProtocolGemini, body: `{"contents":[{"parts":[{"text":"safe","text":"unsafe"}]}]}`},
		{name: "embeddings top level", protocol: service.ContentModerationProtocolOpenAIEmbeddings, body: `{"input":"safe","input":[1234]}`},
		{name: "alpha search nested", protocol: service.ContentModerationProtocolOpenAIAlphaSearch, body: `{"commands":{"search_query":[{"q":"safe","q":"unsafe"}]}}`},
		{name: "images top level", protocol: service.ContentModerationProtocolOpenAIImages, body: `{"prompt":"safe","prompt":"unsafe"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/test", nil)

			decision := runSecurityAudit(
				c,
				nil,
				nil,
				nil,
				&service.APIKey{},
				middleware2.AuthSubject{},
				tc.protocol,
				"test-model",
				[]byte(tc.body),
				"http",
			)
			require.NotNil(t, decision)
			require.False(t, decision.AllowNextStage)
			require.Equal(t, http.StatusBadRequest, decision.HTTPStatus)
			require.Equal(t, service.DuplicateJSONKeysMessage, decision.ClientMessage)
			require.False(t, recorder.Result().Header.Get("Content-Type") != "", "audit must not write before handler formats the error")

			(&OpenAIGatewayHandler{}).openAISecurityAuditError(c, decision)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Contains(t, recorder.Body.String(), service.DuplicateJSONKeysMessage)
		})
	}
}
