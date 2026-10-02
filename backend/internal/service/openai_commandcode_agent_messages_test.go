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

// Exercise Forward, rather than only the helper, to detect an unwired adapter.
func TestForwardCommandCodeAgentMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, baseURL string
		wantPortable  bool
		customTool    bool
	}{
		{"commandcode", "https://api.commandcode.ai/provider/v1", true, false},
		{"commandcode_custom_tool", "https://api.commandcode.ai/provider/v1", true, true},
		{"official_openai", "https://api.openai.com/v1", false, false},
		{"other_provider", "https://example.com/v1", false, false},
		{"hostname_suffix", "https://api.commandcode.ai.example.com/v1", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"resp_test","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "test", "base_url": tt.baseURL},
				Extra:       map[string]any{"openai_responses_mode": "force_responses"},
			}
			body := []byte(`{"model":"deepseek/deepseek-v4.1-flash","store":false,"stream":false,"input":[
				{"type":"message","role":"user","content":"ordinary user text"},
				{"type":"agent_message","id":"amsg_first","author":"/root","recipient":"/root/test","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},{"type":"encrypted_content","encrypted_content":"FIRST_729 read the image"}]},
				{"type":"agent_message","id":"amsg_followup","author":"/root","recipient":"/root/test","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},{"type":"encrypted_content","encrypted_content":"SECOND_428 use the previous image"}]},
				{"type":"reasoning","encrypted_content":"gAAAA_real_reasoning","summary":[]},
				{"type":"function_call","call_id":"call_image","name":"view_image","arguments":"{}"},
				{"type":"function_call_output","call_id":"call_image","output":[{"type":"input_image","image_url":"data:image/png;base64,AQID"}]}
			]}`)
			if tt.customTool {
				body = []byte(strings.ReplaceAll(strings.ReplaceAll(string(body), "function_call", "custom_tool_call"), `"arguments":"{}"`, `"input":"view_image()"`))
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			_, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			out := upstream.lastBody
			require.NotEmpty(t, out)
			for i, token := range map[string]string{"1": "FIRST_729 read the image", "2": "SECOND_428 use the previous image"} {
				item := gjson.GetBytes(out, "input."+i)
				if tt.wantPortable {
					require.Equal(t, "message", item.Get("type").String())
					require.Equal(t, "user", item.Get("role").String())
					require.Equal(t, token, item.Get("content.1.text").String())
					require.False(t, item.Get("content.1.encrypted_content").Exists())
				} else {
					require.Equal(t, "agent_message", item.Get("type").String())
					require.Equal(t, token, item.Get("content.1.encrypted_content").String())
				}
				require.Equal(t, "/root", item.Get("author").String())
				require.Equal(t, "/root/test", item.Get("recipient").String())
			}
			require.Equal(t, "ordinary user text", gjson.GetBytes(out, "input.0.content").String())
			require.Equal(t, "gAAAA_real_reasoning", gjson.GetBytes(out, "input.3.encrypted_content").String())
			if tt.wantPortable {
				require.Contains(t, gjson.GetBytes(out, "input.5.output").String(), "Tool output media moved")
				require.Equal(t, "call_image", gjson.GetBytes(out, "input.5.call_id").String())
				require.Equal(t, "user", gjson.GetBytes(out, "input.6.role").String())
				require.Contains(t, gjson.GetBytes(out, "input.6.content.0.text").String(), "call_image")
				require.Equal(t, "data:image/png;base64,AQID", gjson.GetBytes(out, "input.6.content.1.image_url").String())
			} else {
				require.Equal(t, "data:image/png;base64,AQID", gjson.GetBytes(out, "input.5.output.0.image_url").String())
			}
		})
	}
}
