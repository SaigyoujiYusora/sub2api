package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardCommandCodeDeepSeekClientToolScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, host, model, mapped string
		lower                     bool
	}{
		{"deepseek_alias", "https://api.commandcode.ai/provider/v1", "deepseek-v4.1-flash", "deepseek/deepseek-v4.1-flash", true},
		{"custom_model_mapping", "https://api.commandcode.ai/provider/v1", "my-ds", "deepseek/deepseek-v4.1-flash", true},
		{"commandcode_gpt", "https://api.commandcode.ai/provider/v1", "gpt-5.4", "", false},
		{"other_provider", "https://relay.example/v1", "deepseek-v4.1-flash", "", false},
		{"hostname_suffix", "https://api.commandcode.ai.example/v1", "deepseek-v4.1-flash", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_scope","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))}}
			svc := openAIClientToolsTestService(upstream)
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "test", "base_url": tt.host}, Extra: map[string]any{"openai_responses_mode": "force_responses"}}
			if tt.mapped != "" {
				account.Credentials["model_mapping"] = map[string]any{tt.model: tt.mapped}
			}
			body := []byte(strings.ReplaceAll(string(openAIClientToolsRequest(false)), "gpt-5.4", tt.model))
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			_, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			if tt.lower {
				assertOpenAIClientToolsLowered(t, upstream.lastBody)
			} else {
				require.Equal(t, "custom", gjson.GetBytes(upstream.lastBody, "tools.0.type").String())
			}
			wantModel := tt.model
			if tt.mapped != "" {
				wantModel = tt.mapped
			}
			require.Equal(t, wantModel, gjson.GetBytes(upstream.lastBody, "model").String())
			require.Equal(t, tt.host+"/responses", upstream.lastReq.URL.String())
		})
	}
}

func TestForwardCommandCodeDeepSeekClientToolRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"model":"deepseek-v4.1-flash","store":false,"stream":false,"tools":[{"type":"custom","name":"exec"},{"type":"namespace","name":"collaboration","description":"Agent tools","tools":[{"type":"function","name":"list_agents","parameters":{"type":"object","properties":{}}}]}],"input":[{"type":"agent_message","author":"/root","recipient":"/root/image","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},{"type":"encrypted_content","encrypted_content":"IMAGE_TASK_4827"}]},{"type":"custom_tool_call","call_id":"call_previous","name":"exec","input":"view_image()"},{"type":"custom_tool_call_output","call_id":"call_previous","output":[{"type":"input_text","text":"RESULT_729"},{"type":"input_image","image_url":"data:image/png;base64,AQID"}]}]}`)
			response := `{"id":"resp_roundtrip","status":"completed","output":[{"id":"fc_next","type":"function_call","call_id":"call_next","name":"exec","arguments":"{\"input\":\"text('OK');\"}"},{"id":"fc_agent","type":"function_call","call_id":"call_agent","name":"collaboration__list_agents","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}`
			contentType := "application/json"
			if stream {
				body = []byte(strings.Replace(string(body), `"stream":false`, `"stream":true`, 1))
				response = strings.Join([]string{
					`data: {"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"function_call","id":"fc_next","call_id":"call_next","name":"exec","status":"in_progress"}}`,
					`data: {"type":"response.function_call_arguments.delta","sequence_number":1,"item_id":"fc_next","output_index":0,"delta":"{\"input\":\"text('OK');\"}"}`,
					`data: {"type":"response.function_call_arguments.done","sequence_number":2,"item_id":"fc_next","output_index":0,"arguments":"{\"input\":\"text('OK');\"}"}`,
					`data: {"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"type":"function_call","id":"fc_next","call_id":"call_next","name":"exec","arguments":"{\"input\":\"text('OK');\"}","status":"completed"}}`,
					`data: {"type":"response.completed","sequence_number":4,"response":` + response + `}`,
				}, "\n\n") + "\n\n"
				contentType = "text/event-stream"
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(response))}}
			svc := openAIClientToolsTestService(upstream)
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "test", "base_url": "https://api.commandcode.ai/provider/v1", "model_mapping": map[string]any{"deepseek-v4.1-flash": "deepseek/deepseek-v4.1-flash"}}, Extra: map[string]any{"openai_responses_mode": "force_responses"}}
			writer := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			_, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.Equal(t, "deepseek/deepseek-v4.1-flash", gjson.GetBytes(upstream.lastBody, "model").String())
			require.Equal(t, "function", gjson.GetBytes(upstream.lastBody, "tools.0.type").String())
			require.Equal(t, "message", gjson.GetBytes(upstream.lastBody, "input.0.type").String())
			require.Contains(t, gjson.GetBytes(upstream.lastBody, "input.0.content").Raw, "IMAGE_TASK_4827")
			require.Equal(t, "function_call", gjson.GetBytes(upstream.lastBody, "input.1.type").String())
			require.Equal(t, "view_image()", gjson.Get(gjson.GetBytes(upstream.lastBody, "input.1.arguments").String(), "input").String())
			require.Contains(t, gjson.GetBytes(upstream.lastBody, "input.2.output").String(), "RESULT_729")
			require.Equal(t, "data:image/png;base64,AQID", gjson.GetBytes(upstream.lastBody, "input.3.content.1.image_url").String())
			out := writer.Body.String()
			require.Contains(t, out, `"type":"custom_tool_call"`)
			require.Contains(t, out, `"input":"text('OK');"`)
			require.Contains(t, out, `"namespace":"collaboration"`)
			require.Contains(t, out, `"name":"list_agents"`)
			if stream {
				require.Contains(t, out, `"type":"response.custom_tool_call_input.done"`)
			}
		})
	}
}
