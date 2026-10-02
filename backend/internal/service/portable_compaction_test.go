//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

const portableTestSummary = "Goal: update config. Completed: inspected settings. Next: verify the change."

func portableTestResponse(status string) string {
	return fmt.Sprintf(`{"id":"resp_summary","object":"response","status":%q,"model":"fixture","output":[{"id":"msg_summary","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":17,"output_tokens":8,"total_tokens":25}}`, status, portableTestSummary)
}

func portableTestHTTPResponse(body, contentType string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

func portableTestInput(model string, stream bool) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"stream":%t,"instructions":"Keep the user constraints.","reasoning":{"effort":"high"},"tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}],"tool_choice":"required","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"update config"}]},{"type":"additional_tools","tools":[]},{"type":"compaction_trigger"}]}`, model, stream))
}

func portableTestContext(body []byte, path string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

func portableTestFinal(t *testing.T, rec *httptest.ResponseRecorder) gjson.Result {
	t.Helper()
	if strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		var final gjson.Result
		done := 0
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			event := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if event.Get("type").String() == "response.output_item.done" {
				done++
				require.Equal(t, "compaction", event.Get("item.type").String())
			}
			if event.Get("type").String() == "response.completed" {
				final = event.Get("response")
			}
		}
		require.Equal(t, 1, done)
		require.True(t, final.Exists(), rec.Body.String())
		return final
	}
	return gjson.Parse(rec.Body.String())
}

func portableTestPacket(t *testing.T) string {
	t.Helper()
	body, err := buildPortableCompactionResponse([]byte(portableTestResponse("completed")), "grok-4.7")
	require.NoError(t, err)
	return gjson.GetBytes(body, "output.0.encrypted_content").String()
}

func TestPortableCompactionReplayAndValidation(t *testing.T) {
	packet := portableTestPacket(t)
	for _, native := range []bool{false, true} {
		body := []byte(fmt.Sprintf(`{"model":"target","input":[{"type":"compaction","encrypted_content":%q},{"type":"message","role":"user","content":"continue"}],"counter":900719925474099312345}`, packet))
		expanded, changed, err := ExpandPortableCompactionInputs(body, native)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "user", gjson.GetBytes(expanded, "input.0.role").String())
		require.Contains(t, gjson.GetBytes(expanded, "input.0.content.0.text").String(), portableTestSummary)
		require.NotContains(t, string(expanded), "encrypted_content")
		require.Equal(t, "900719925474099312345", gjson.GetBytes(expanded, "counter").Raw)
	}
	unknown := []byte(`{"input":[{"type":"\u0063ompaction","encrypted_content":"native-cipher"}]}`)
	_, _, err := ExpandPortableCompactionInputs(unknown, false)
	require.ErrorContains(t, err, "cannot read native encrypted")
	unchanged, changed, err := ExpandPortableCompactionInputs(unknown, true)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, unknown, unchanged)
	for _, data := range []string{`{"version":2,"model":"grok","summary":"ok"}`, `{"version":1,"model":"grok","summary":""}`, `{"version":1,"model":"grok","summary":"ok","extra":true}`, `{"version":1,"model":"grok","summary":"ok"} {}`} {
		_, err := decodePortableCompaction(portableCompactionPrefix + base64.RawURLEncoding.EncodeToString([]byte(data)))
		require.Error(t, err)
	}
	_, err = decodePortableCompaction(portableCompactionPrefix + "bad!")
	require.Error(t, err)
	_, err = decodePortableCompaction(portableCompactionPrefix + strings.Repeat("A", 2*portableCompactionSummaryLimit))
	require.Error(t, err)
	_, _, err = ExpandPortableCompactionInputs([]byte(fmt.Sprintf(`{"previous_response_id":"resp_prev","input":[{"type":"compaction","encrypted_content":%q}]}`, packet)), true)
	require.ErrorContains(t, err, "stateless")
}

func TestPortableCompactionClassifiesActualProviderAndModel(t *testing.T) {
	openai := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-luna", "o3", "o4-mini", "codex-auto-review"} {
		require.True(t, UsesNativeGPTCompaction(openai, model), model)
	}
	for _, model := range []string{"grok-4.7", "deepseek-v4.1-flash", "gpt-image-2", "gpt-oss-120b", "orchestrator-astra"} {
		require.False(t, UsesNativeGPTCompaction(openai, model), model)
	}
	require.False(t, UsesNativeGPTCompaction(&Account{Platform: PlatformAntigravity}, "gpt-6-astra"))
	deepseek := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://api.deepseek.com"}}
	require.False(t, UsesNativeGPTCompaction(deepseek, "gpt-6-astra"))
	require.False(t, UsesNativeGPTCompaction(deepseek, codexAutoReviewModel))
	require.False(t, UsesNativeGPTCompaction(&Account{Platform: PlatformGrok}, codexAutoReviewModel))
}

func TestPortableCompactionSummaryOmitsBackground(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		body := []byte(`{"model":"grok-4.7","background":` + value + `,"input":[{"type":"message","role":"user","content":"preserve this"},{"type":"compaction_trigger"}]}`)
		summary, err := buildPortableSummaryRequest(body)
		require.NoError(t, err)
		require.False(t, gjson.GetBytes(summary, "background").Exists())
	}
}

func TestPortableCompactionAutoReviewNativeHistory(t *testing.T) {
	for _, target := range []string{codexAutoReviewModel, "gpt-6-luna", "deepseek-v4.1-flash"} {
		t.Run(target, func(t *testing.T) {
			account := rawChatCompletionsTestAccount()
			account.Extra = map[string]any{"openai_responses_mode": "force_responses"}
			account.Credentials["model_mapping"] = map[string]any{codexAutoReviewModel: target}
			upstream := &httpUpstreamRecorder{resp: portableTestHTTPResponse(portableTestResponse("completed"), "application/json")}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			body := []byte(`{"model":"codex-auto-review","input":[{"type":"compaction","encrypted_content":"native-cipher"},{"type":"message","role":"user","content":"review this command"}]}`)
			c, rec := portableTestContext(body, "/v1/responses")
			_, err := svc.Forward(context.Background(), c, account, body)
			wsBody, wsErr := preparePortableCompactionWebSocketInput(body, account, target)
			if target == "deepseek-v4.1-flash" {
				require.ErrorContains(t, err, "cannot read native encrypted")
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.Empty(t, upstream.bodies)
				require.ErrorContains(t, wsErr, "cannot read native encrypted")
				return
			}
			require.NoError(t, err, rec.Body.String())
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, target, gjson.GetBytes(upstream.lastBody, "model").String())
			require.Equal(t, "native-cipher", gjson.GetBytes(upstream.lastBody, "input.0.encrypted_content").String())
			require.NoError(t, wsErr)
			require.Equal(t, body, wsBody)
		})
	}
}

func TestPortableCompactionOpenAIServiceEntries(t *testing.T) {
	for _, route := range []string{"responses", "grok", "deepseek-chat"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", route, stream), func(t *testing.T) {
				account := rawChatCompletionsTestAccount()
				account.Extra = map[string]any{"openai_responses_mode": "force_responses"}
				model := "deepseek-v4.1-flash"
				response := portableTestHTTPResponse(portableTestResponse("completed"), "application/json")
				if route == "grok" {
					account.Platform = PlatformGrok
					model = "grok-4.7"
				}
				if route == "deepseek-chat" {
					account.Extra = map[string]any{"openai_responses_mode": "force_chat_completions"}
					response = portableTestHTTPResponse(`{"id":"chatcmpl_summary","model":"deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","content":"`+portableTestSummary+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":8,"total_tokens":25}}`, "application/json")
				}
				upstream := &httpUpstreamRecorder{resp: response}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				body := portableTestInput(model, stream)
				c, rec := portableTestContext(body, "/v1/responses")
				if stream {
					MarkOpenAINativeCompactionV2(c)
					MarkOpenAICompactClientStream(c)
				}
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err, rec.Body.String())
				require.NotNil(t, result)
				require.Equal(t, 17, result.Usage.InputTokens)
				require.Equal(t, 8, result.Usage.OutputTokens)
				require.Equal(t, stream, result.Stream)
				require.Len(t, upstream.bodies, 1)
				if route == "deepseek-chat" {
					require.Equal(t, "/v1/chat/completions", upstream.lastReq.URL.Path)
				} else {
					require.Contains(t, upstream.lastReq.URL.Path, "/responses")
				}
				require.NotContains(t, string(upstream.lastBody), "compaction_trigger")
				require.NotContains(t, string(upstream.lastBody), "additional_tools")
				require.NotContains(t, string(upstream.lastBody), `"tool_choice":"required"`)
				require.False(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
				require.False(t, gjson.GetBytes(upstream.lastBody, "background").Exists())
				require.Contains(t, string(upstream.lastBody), "Return only the summary")
				final := portableTestFinal(t, rec)
				require.Len(t, final.Get("output").Array(), 1)
				envelope, err := decodePortableCompaction(final.Get("output.0.encrypted_content").String())
				require.NoError(t, err)
				require.Equal(t, portableTestSummary, envelope.Summary)
				require.Equal(t, model, envelope.Model)
			})
		}
	}
}

func TestPortableCompactionGPTNativeAndMappedAliases(t *testing.T) {
	for _, requested := range []string{"gpt-6-astra", "orchestrator-astra"} {
		account := rawChatCompletionsTestAccount()
		account.Extra = map[string]any{"openai_responses_mode": "force_responses"}
		account.Credentials["model_mapping"] = map[string]any{"orchestrator-astra": "gpt-6-astra"}
		upstream := &httpUpstreamRecorder{resp: portableTestHTTPResponse(`{"id":"resp_native","status":"completed","output":[{"type":"compaction","encrypted_content":"native-cipher"}],"usage":{"input_tokens":17,"output_tokens":8}}`, "application/json")}
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		body := portableTestInput(requested, false)
		c, rec := portableTestContext(body, "/v1/responses")
		MarkOpenAINativeCompactionV2(c)
		result, err := svc.Forward(context.Background(), c, account, body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, "gpt-6-astra", gjson.GetBytes(upstream.lastBody, "model").String())
		require.Contains(t, rec.Body.String(), "native-cipher")
		require.NotContains(t, string(upstream.lastBody), portableCompactionPrompt)
	}
}

func TestPortableCompactionLegacyMappingAndReplayToGPT(t *testing.T) {
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{"openai_responses_mode": "force_responses"}
	account.Credentials["model_mapping"] = map[string]any{"public-model": "gpt-6-astra", "deepseek-v4.1-flash": "gpt-6-sol"}
	account.Credentials["compact_model_mapping"] = map[string]any{"public-model": "deepseek-v4.1-flash"}
	upstream := &httpUpstreamRecorder{resp: portableTestHTTPResponse(portableTestResponse("completed"), "application/json")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	body := []byte(`{"model":"public-model","input":"update config"}`)
	c, rec := portableTestContext(body, "/v1/responses/compact")
	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, "deepseek-v4.1-flash", gjson.GetBytes(upstream.lastBody, "model").String())
	require.NotContains(t, upstream.lastReq.URL.Path, "/compact")
	require.Equal(t, "gpt-6-astra", account.GetMappedModel("public-model"))
	packet := portableTestFinal(t, rec).Get("output.0.encrypted_content").String()
	upstream.resp = portableTestHTTPResponse(portableTestResponse("completed"), "application/json")
	body = []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":[{"type":"compaction","encrypted_content":%q},{"type":"message","role":"user","content":"continue"}]}`, packet))
	c, _ = portableTestContext(body, "/v1/responses")
	_, err = svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Contains(t, string(upstream.lastBody), portableTestSummary)
	require.NotContains(t, string(upstream.lastBody), portableCompactionPrefix)
}

func TestPortableCompactionRejectsIncompleteAndKeepsMeteredUsage(t *testing.T) {
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{"openai_responses_mode": "force_responses"}
	upstream := &httpUpstreamRecorder{resp: portableTestHTTPResponse(portableTestResponse("incomplete"), "application/json")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	body := portableTestInput("grok-4.7", false)
	c, rec := portableTestContext(body, "/v1/responses")
	result, err := svc.Forward(context.Background(), c, account, body)
	require.True(t, IsPortableCompactionResponseError(err), "%v", err)
	require.NotNil(t, result)
	require.Equal(t, 17, result.Usage.InputTokens)
	require.NotContains(t, rec.Body.String(), portableCompactionPrefix)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	for _, invalid := range []string{`{"status":"completed","output":[{"type":"function_call","name":"exec"}]}`, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]}`, `{"status":"completed","output":[]}`} {
		_, err := buildPortableCompactionResponse([]byte(invalid), "grok")
		require.Error(t, err)
	}
}

func TestPortableCompactionCaptureContextAndCancellation(t *testing.T) {
	body := portableTestInput("grok-4.7", true)
	c, rec := portableTestContext(body, "/v1/responses/compact")
	MarkOpenAINativeCompactionV2(c)
	MarkOpenAICompactClientStream(c)
	result, err := forwardPortableCompaction(context.Background(), c, body, "grok-4.7", func(ctx context.Context, inner *gin.Context, summary []byte) (*OpenAIForwardResult, error) {
		require.Equal(t, "/v1/responses", inner.Request.URL.Path)
		require.False(t, isOpenAINativeCompactionV2(inner))
		require.False(t, openAICompactClientWantsStream(inner))
		require.False(t, gjson.GetBytes(summary, "stream").Bool())
		inner.Set(OpsUpstreamModelKey, "grok-4.7")
		inner.Data(http.StatusOK, "application/json", []byte(portableTestResponse("completed")))
		return &OpenAIForwardResult{Usage: OpenAIUsage{InputTokens: 17}}, nil
	})
	require.NoError(t, err)
	require.Equal(t, 17, result.Usage.InputTokens)
	require.Equal(t, "grok-4.7", c.GetString(OpsUpstreamModelKey))
	portableTestFinal(t, rec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = forwardPortableCompaction(ctx, c, body, "grok", func(context.Context, *gin.Context, []byte) (*OpenAIForwardResult, error) {
		t.Fatal("canceled request must not invoke an upstream")
		return nil, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	writer := &portableCompactionWriter{header: make(http.Header), status: 200}
	_, err = writer.Write(make([]byte, portableCompactionCaptureLimit+1))
	require.Error(t, err)
	require.True(t, writer.overflow)
	require.Zero(t, writer.buffer.Len())
}

func TestPortableCompactionWebSocketReplayAndExplicitBoundary(t *testing.T) {
	packet := portableTestPacket(t)
	account := &Account{Platform: PlatformOpenAI}
	for _, model := range []string{"gpt-6-astra", "grok-4.7"} {
		body := []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"type":"compaction","encrypted_content":%q}]}`, model, packet))
		expanded, err := preparePortableCompactionWebSocketInput(body, account, model)
		require.NoError(t, err)
		require.Contains(t, string(expanded), portableTestSummary)
		require.NotContains(t, string(expanded), "encrypted_content")
	}
	_, err := preparePortableCompactionWebSocketInput(portableTestInput("grok-4.7", true), account, "grok-4.7")
	require.ErrorContains(t, err, "HTTP POST")
	_, err = preparePortableCompactionWebSocketInput(portableTestInput("gpt-6-astra", true), account, "gpt-6-astra")
	require.NoError(t, err)
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	body := []byte(`{"type":"response.create","model":"grok-4.7","input":[{"type":"compaction","encrypted_content":"native-cipher"}]}`)
	c, _ := portableTestContext(body, "/v1/responses")
	_, err = svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "token", body, len(body), "grok-4.7", "", "", "", "", 1, func([]byte) error { return errors.New("must not write") })
	require.ErrorContains(t, err, "cannot read native encrypted")
	require.Empty(t, upstream.requests)
}

func TestPortableCompactionAntigravityServiceEntry(t *testing.T) {
	for _, stream := range []bool{false, true} {
		upstream := &httpUpstreamRecorder{resp: antigravityCompatSuccessResponse()}
		svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
		body := portableTestInput("gemini-3.1-pro-high", stream)
		c, rec := portableTestContext(body, "/v1/responses")
		result, err := svc.ForwardAsResponses(context.Background(), c, newAntigravityCompatAccount(AccountTypeOAuth), body, nil)
		require.NoError(t, err, rec.Body.String())
		require.NotNil(t, result)
		require.Equal(t, 8, result.Usage.InputTokens)
		envelope, err := decodePortableCompaction(portableTestFinal(t, rec).Get("output.0.encrypted_content").String())
		require.NoError(t, err)
		require.Equal(t, "ok", envelope.Summary)
		require.Contains(t, string(upstream.lastBody), "Return only the summary")
		require.NotContains(t, string(upstream.lastBody), "compaction_trigger")
	}
}

func TestPortableCompactionAnthropicServiceEntryUsesValidThinkingBudget(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_summary\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":17}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"summary\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, effort := range []string{"medium", "high", "xhigh"} {
		upstream := &anthropicHTTPUpstreamRecorder{resp: portableTestHTTPResponse(stream, "text/event-stream")}
		svc := &GatewayService{cfg: &config.Config{}, httpUpstream: upstream}
		account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-key"}}
		body := bytes.ReplaceAll(portableTestInput("claude-sonnet-4-6", false), []byte(`"high"`), []byte(fmt.Sprintf("%q", effort)))
		c, rec := portableTestContext(body, "/v1/responses")
		result, err := svc.ForwardAsResponses(context.Background(), c, account, body, nil)
		require.NoError(t, err, rec.Body.String())
		require.NotNil(t, result)
		require.Equal(t, 4096, int(gjson.GetBytes(upstream.lastBody, "max_tokens").Int()))
		require.False(t, gjson.GetBytes(upstream.lastBody, "thinking.budget_tokens").Exists())
		require.Equal(t, "low", gjson.GetBytes(upstream.lastBody, "output_config.effort").String())
		envelope, err := decodePortableCompaction(portableTestFinal(t, rec).Get("output.0.encrypted_content").String())
		require.NoError(t, err)
		require.Equal(t, "summary", envelope.Summary)
	}
}

func TestPortableCompactionRejectsUnknownCipherBeforeInference(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","input":[{"type":"compaction","encrypted_content":"native-cipher"}]}`)
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	c, rec := portableTestContext(body, "/v1/responses")
	_, err := svc.Forward(context.Background(), c, rawChatCompletionsTestAccount(), body)
	require.ErrorContains(t, err, "cannot read native encrypted")
	require.Equal(t, 400, rec.Code)
	require.Empty(t, upstream.requests)
}

func TestPortableCompactionEnvelopeRejectsOversizedSummary(t *testing.T) {
	data, err := json.Marshal(portableCompactionEnvelope{Version: 1, Model: "grok", Summary: strings.Repeat("x", portableCompactionSummaryLimit+1)})
	require.NoError(t, err)
	_, err = decodePortableCompaction(portableCompactionPrefix + base64.RawURLEncoding.EncodeToString(data))
	require.Error(t, err)
	summary := strings.Repeat("\x00", portableCompactionSummaryLimit-1) + "x"
	var response map[string]any
	require.NoError(t, json.Unmarshal([]byte(portableTestResponse("completed")), &response))
	response["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = summary
	data, err = json.Marshal(response)
	require.NoError(t, err)
	encoded, err := buildPortableCompactionResponse(data, "grok")
	require.NoError(t, err)
	envelope, err := decodePortableCompaction(gjson.GetBytes(encoded, "output.0.encrypted_content").String())
	require.NoError(t, err)
	require.Equal(t, summary, envelope.Summary)
}

func TestPortableCompactionPassthroughGlobalMapping(t *testing.T) {
	for _, target := range []string{"gpt-6-astra", "deepseek-v4.1-flash"} {
		account := rawChatCompletionsTestAccount()
		account.Extra = map[string]any{"openai_passthrough": true, "openai_responses_mode": "force_responses"}
		response := portableTestResponse("completed")
		if strings.HasPrefix(target, "gpt-") {
			response = `{"id":"resp_native","status":"completed","output":[{"type":"compaction","encrypted_content":"native-cipher"}],"usage":{"input_tokens":17,"output_tokens":8}}`
		}
		upstream := &httpUpstreamRecorder{resp: portableTestHTTPResponse(response, "application/json")}
		cfg := rawChatCompletionsTestConfig()
		cfg.Gateway.OpenAICompactModel = target
		svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
		body := []byte(`{"model":"gpt-6-sol","input":"update config"}`)
		c, rec := portableTestContext(body, "/v1/responses/compact")
		_, err := svc.Forward(context.Background(), c, account, body)
		require.NoError(t, err)
		require.Equal(t, target, gjson.GetBytes(upstream.lastBody, "model").String())
		if strings.HasPrefix(target, "gpt-") {
			require.Contains(t, upstream.lastReq.URL.Path, "/compact")
			require.Contains(t, rec.Body.String(), "native-cipher")
		} else {
			require.NotContains(t, upstream.lastReq.URL.Path, "/compact")
			_, err := decodePortableCompaction(portableTestFinal(t, rec).Get("output.0.encrypted_content").String())
			require.NoError(t, err)
		}
	}
}

func TestPortableCompactionRejectsTruncatedGeminiStream(t *testing.T) {
	for _, finish := range []string{"", "SAFETY", "MAX_TOKENS"} {
		response := fmt.Sprintf("data: {\"response\":{\"responseId\":\"resp_partial\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial summary\"}]},\"finishReason\":%q}],\"usageMetadata\":{\"promptTokenCount\":17,\"candidatesTokenCount\":8}}}\n\n", finish)
		upstream := &httpUpstreamRecorder{resp: portableTestHTTPResponse(response, "text/event-stream")}
		svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
		body := portableTestInput("gemini-3.1-pro-high", true)
		c, rec := portableTestContext(body, "/v1/responses")
		result, err := svc.ForwardAsResponses(context.Background(), c, newAntigravityCompatAccount(AccountTypeOAuth), body, nil)
		require.True(t, IsPortableCompactionResponseError(err), "%s: %v", finish, err)
		require.NotNil(t, result)
		require.Equal(t, 17, result.Usage.InputTokens)
		require.Equal(t, 8, result.Usage.OutputTokens)
		require.NotContains(t, rec.Body.String(), portableCompactionPrefix)
	}
}

func TestPortableCompactionRejectsTruncatedAnthropicStream(t *testing.T) {
	response := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_partial\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":17}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"partial summary\"}}\n\n"
	upstream := &anthropicHTTPUpstreamRecorder{resp: portableTestHTTPResponse(response, "text/event-stream")}
	svc := &GatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-key"}}
	body := portableTestInput("claude-sonnet-4-6", true)
	c, rec := portableTestContext(body, "/v1/responses")
	result, err := svc.ForwardAsResponses(context.Background(), c, account, body, nil)
	require.True(t, IsPortableCompactionResponseError(err), "%v", err)
	require.NotNil(t, result)
	require.Equal(t, 17, result.Usage.InputTokens)
	require.NotContains(t, rec.Body.String(), portableCompactionPrefix)
}

func TestPortableCompactionRejectsUnfinishedChatCompletion(t *testing.T) {
	for _, finish := range []string{"", "length", "content_filter"} {
		response := fmt.Sprintf(`{"id":"chatcmpl_partial","model":"deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","content":"partial summary"},"finish_reason":%q}],"usage":{"prompt_tokens":17,"completion_tokens":8,"total_tokens":25}}`, finish)
		upstream := &httpUpstreamRecorder{resp: portableTestHTTPResponse(response, "application/json")}
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		account := rawChatCompletionsTestAccount()
		account.Extra = map[string]any{"openai_responses_mode": "force_chat_completions"}
		body := portableTestInput("deepseek-v4.1-flash", false)
		c, rec := portableTestContext(body, "/v1/responses")
		result, err := svc.Forward(context.Background(), c, account, body)
		require.True(t, IsPortableCompactionResponseError(err), "%s: %v", finish, err)
		require.NotNil(t, result)
		require.Equal(t, 17, result.Usage.InputTokens)
		require.NotContains(t, rec.Body.String(), portableCompactionPrefix)
	}
}

func TestPortableCompactionNativeFallbackCannotSendCipherToNonGPT(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		account := rawChatCompletionsTestAccount()
		account.Extra = map[string]any{"openai_responses_mode": "force_responses", "openai_passthrough": passthrough}
		cfg := rawChatCompletionsTestConfig()
		cfg.Gateway.OpenAICompactModel = "deepseek-v4.1-flash"
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"model_not_found","message":"The model gpt-6-astra is not available for this account"}}`))}}
		svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
		body := []byte(`{"model":"gpt-6-astra","stream":false,"input":[{"type":"compaction","encrypted_content":"native-cipher"},{"type":"compaction_trigger"}]}`)
		c, _ := portableTestContext(body, "/v1/responses")
		MarkOpenAINativeCompactionV2(c)
		_, err := svc.Forward(context.Background(), c, account, body)
		require.Error(t, err)
		require.Len(t, upstream.bodies, 1)
		require.Equal(t, "gpt-6-astra", gjson.GetBytes(upstream.lastBody, "model").String())
		_, _, retry := svc.prepareOpenAICompactFallbackRetry(c, account, "gpt-6-astra", body, 400, "The model is not available", []byte(`{"error":{"code":"model_not_found"}}`), false)
		require.False(t, retry)
		cfg.Gateway.OpenAICompactModel = "gpt-6-sol"
		_, target, retry := svc.prepareOpenAICompactFallbackRetry(c, account, "gpt-6-astra", body, 400, "The model is not available", []byte(`{"error":{"code":"model_not_found"}}`), false)
		require.True(t, retry)
		require.Equal(t, "gpt-6-sol", target)
	}
}
