package service

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

const CarpoolMaskedUpstreamClientMessage = "上游服务暂时不可用，请稍后重试；若持续失败请联系管理员。"

// recordOpenAINonFailoverHTTPError keeps the provider response available to
// ops diagnostics while the client receives only the masked error envelope.
func (s *OpenAIGatewayService) recordOpenAINonFailoverHTTPError(c *gin.Context, account *Account, resp *http.Response, body []byte) {
	if account == nil || resp == nil {
		return
	}
	upstreamMessage := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(body))
	if upstreamMessage == "" {
		upstreamMessage = fmt.Sprintf("upstream returned status %d", resp.StatusCode)
	}
	upstreamDetail := ""
	if s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMessage, upstreamDetail)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               "http_error",
		Message:            upstreamMessage,
		Detail:             upstreamDetail,
	})
}

func writeCarpoolMaskedUpstreamError(c *gin.Context) {
	if c == nil || c.Writer.Written() {
		return
	}
	MarkResponseCommitted(c)
	c.JSON(http.StatusBadGateway, gin.H{
		"error": gin.H{
			"type":    "upstream_error",
			"message": CarpoolMaskedUpstreamClientMessage,
		},
	})
}
