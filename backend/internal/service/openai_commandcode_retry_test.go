package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestForwardCommandCodeTransient400Retry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, base, code, param, message string
		failures, attempts               int
		wantError                        bool
	}{
		{"recovers", "https://api.commandcode.ai/provider/v1", "", "", "invalid request error trace_id: e3ef771c0d85aac733a1093560541d47", 1, 2, false},
		{"recovers_second_retry", "https://api.commandcode.ai/provider/v1", "", "", "invalid request error trace_id: e3ef771c0d85aac733a1093560541d47", 2, 3, false},
		{"bounded", "https://api.commandcode.ai/provider/v1", "", "", "invalid request error trace_id: e3ef771c0d85aac733a1093560541d47", 3, 3, true},
		{"specific_code", "https://api.commandcode.ai/provider/v1", "invalid_model", "", "invalid request error trace_id: e3ef771c0d85aac733a1093560541d47", 1, 1, true},
		{"specific_param", "https://api.commandcode.ai/provider/v1", "", "input", "invalid request error trace_id: e3ef771c0d85aac733a1093560541d47", 1, 1, true},
		{"specific_message", "https://api.commandcode.ai/provider/v1", "", "", "Invalid input: missing tool output", 1, 1, true},
		{"other_host", "https://example.com/v1", "", "", "invalid request error trace_id: e3ef771c0d85aac733a1093560541d47", 1, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			inner, _ := json.Marshal(map[string]any{"type": "invalid_request_error", "code": tt.code, "param": tt.param, "message": tt.message})
			outer, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": string(inner) + "\n"}})
			responses := make([]*http.Response, 0, tt.failures+1)
			for i := 0; i < tt.failures; i++ {
				responses = append(responses, &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(outer))})
			}
			responses = append(responses, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_ok","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))})
			upstream := &httpUpstreamRecorder{responses: responses}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "test", "base_url": tt.base}, Extra: map[string]any{"openai_responses_mode": "force_responses"}}
			body := []byte(`{"model":"deepseek/deepseek-v4.1-flash","store":false,"stream":false,"input":[{"role":"user","content":"original user request"}]}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			_, err := svc.Forward(context.Background(), c, account, body)
			if tt.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, upstream.requests, tt.attempts)
			for _, sent := range upstream.bodies {
				require.Equal(t, string(upstream.bodies[0]), string(sent), "retry must not rewrite input")
			}
		})
	}
}
