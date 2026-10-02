package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPortableCompactionRejectionDoesNotAppendFallbackError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"deepseek-v4.1-flash","stream":%t,"input":[{"type":"compaction","encrypted_content":"native-cipher"}]}`, stream)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			account := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
			result, err := (&service.OpenAIGatewayService{}).Forward(context.Background(), c, account, []byte(body))
			require.ErrorContains(t, err, "cannot read native encrypted")
			require.Nil(t, result)
			before := recorder.Body.String()
			wroteFallback := (&OpenAIGatewayHandler{}).ensureForwardErrorResponse(c, stream)
			require.False(t, wroteFallback)
			require.Equal(t, before, recorder.Body.String())
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.True(t, json.Valid(recorder.Body.Bytes()))
		})
	}
}
