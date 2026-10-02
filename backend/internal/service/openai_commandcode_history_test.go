package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardCommandCodeInterruptedToolHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, base, previous string
		wantCalls            int
	}{
		{"commandcode", "https://api.commandcode.ai/provider/v1", "", 2},
		{"other_host", "https://example.com/v1", "", 4},
		{"host_suffix", "https://api.commandcode.ai.example.com/v1", "", 4},
		{"server_state", "https://api.commandcode.ai/provider/v1", "resp_previous", 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"id":"resp_test","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "test", "base_url": tt.base},
				Extra:       map[string]any{"openai_responses_mode": "force_responses"}}
			body := []byte(`{"model":"deepseek/deepseek-v4.1-flash","store":false,"stream":false,"previous_response_id":"` + tt.previous + `","seed":9007199254740993123,"input":[
				{"type":"message","role":"user","content":"original request"},
				{"type":"custom_tool_call","call_id":"custom_missing","name":"exec","input":"interrupted"},
				{"type":"custom_tool_call","call_id":"custom_kept","name":"exec","input":"completed"},
				{"type":"message","role":"developer","content":"notice between call and output"},
				{"type":"custom_tool_call_output","call_id":"custom_kept","output":"recorded result"},
				{"type":"function_call","call_id":"function_missing","name":"lookup","arguments":"{}"},
				{"type":"function_call","call_id":"function_kept","name":"lookup","arguments":"{}"},
				{"type":"function_call_output","call_id":"function_kept","output":""},
				{"type":"message","role":"user","content":"latest request after interruption"}
			]}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			_, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			var calls []string
			for _, item := range gjson.GetBytes(upstream.lastBody, "input").Array() {
				if typ := item.Get("type").String(); typ == "function_call" || typ == "custom_tool_call" {
					calls = append(calls, item.Get("call_id").String())
				}
			}
			require.Len(t, calls, tt.wantCalls)
			require.Contains(t, calls, "custom_kept")
			require.Contains(t, calls, "function_kept")
			require.Contains(t, string(upstream.lastBody), "recorded result")
			require.Contains(t, string(upstream.lastBody), "notice between call and output")
			require.Contains(t, string(upstream.lastBody), "latest request after interruption")
			require.Equal(t, "9007199254740993123", gjson.GetBytes(upstream.lastBody, "seed").Raw)
		})
	}
}

func TestCommandCodeToolHistoryPreservesCompleteAndServerReferencedInput(t *testing.T) {
	for _, body := range []string{
		` { "input": [{"type":"custom_tool_call","call_id":"c","name":"exec","input":"keep"},{"type":"custom_tool_call_output","call_id":"c","output":""}], "seed":9007199254740993123 } `,
		`{"input":[{"type":"custom_tool_call","call_id":"c","name":"exec","input":"keep"},{"type":"item_reference","id":"server_item"}]}`,
		`{"conversation":"conv_1","input":[{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"}]}`,
		`{"input":"ordinary text"}`,
		`{"input":[`,
	} {
		require.Equal(t, body, string(dropCommandCodeUnansweredToolCalls([]byte(body))))
	}
}
