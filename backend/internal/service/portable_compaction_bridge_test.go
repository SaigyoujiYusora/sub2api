//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func portableTestBridge() *PortableCompactionBridge {
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.PortableConversionModel = "gpt-6-luna"
	cfg.Gateway.PortableSummaryModelMapping = map[string]string{"grok-*": "gpt-6-luna", "deepseek*": "gpt-6-luna"}
	return NewPortableCompactionBridge(cfg)
}

func TestPortableCompactionBridgeRoutesPlaintextSummary(t *testing.T) {
	for _, model := range []string{"grok-4.7", "deepseek-v4.1-flash"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", model, stream), func(t *testing.T) {
				body := portableTestInput(model, stream)
				body = []byte(strings.Replace(string(body), "{", `{"counter":900719925474099312345,`, 1))
				c, rec := portableTestContext(body, "/v1/responses")
				account := rawChatCompletionsTestAccount()
				if strings.HasPrefix(model, "grok") {
					account.Platform = PlatformGrok
				}
				calls := 0
				portableTestBridge().Bind(c, func(_ context.Context, scope PortableSummaryScope, request []byte) ([]byte, error) {
					calls++
					require.Equal(t, "900719925474099312345", gjson.GetBytes(request, "counter").Raw)
					require.Equal(t, int64(2), scope.APIKeyID)
					require.Equal(t, "gpt-6-luna", gjson.GetBytes(request, "model").String())
					require.False(t, gjson.GetBytes(request, "stream").Bool())
					for _, key := range []string{"background", "tools", "context_management", "previous_response_id"} {
						require.False(t, gjson.GetBytes(request, key).Exists(), key)
					}
					require.NotContains(t, string(request), "compaction_trigger")
					return []byte(portableTestResponse("completed")), nil
				})
				plan := PlanPortableCompaction(c, account, body)
				require.NotNil(t, plan)
				_, done, err := plan.Execute(c, PortableSummaryScope{UserID: 1, APIKeyID: 2, GroupID: 10}, body)
				require.NoError(t, err)
				require.True(t, done)
				require.Equal(t, 1, calls)
				envelope, err := decodePortableCompaction(portableTestFinal(t, rec).Get("output.0.encrypted_content").String())
				require.NoError(t, err)
				require.Equal(t, "gpt-6-luna", envelope.Model)
				require.Equal(t, portableTestSummary, envelope.Summary)
			})
		}
	}
}

func TestPortableCompactionRegularSummaryOnlyUsesConfiguredMapping(t *testing.T) {
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.PortableConversionModel = "gpt-6-luna"
	bridge := NewPortableCompactionBridge(cfg)
	for _, model := range []string{"grok-4.7", "deepseek-v4.1-flash"} {
		body := portableTestInput(model, false)
		c, _ := portableTestContext(body, "/v1/responses")
		bridge.Bind(c, func(context.Context, PortableSummaryScope, []byte) ([]byte, error) {
			t.Fatal("no implicit summary-model call")
			return nil, nil
		})
		account := rawChatCompletionsTestAccount()
		if model == "grok-4.7" {
			account.Platform = PlatformGrok
		}
		require.Nil(t, PlanPortableCompaction(c, account, body), model)
		if model == "grok-4.7" {
			account.Credentials["model_mapping"] = map[string]any{"grok-public": "grok-4.7"}
			account.Credentials["compact_model_mapping"] = map[string]any{"grok-*": "gpt-5.6-luna"}
			body = portableTestInput("grok-public", false)
			plan := PlanPortableCompaction(c, account, body)
			require.NotNil(t, plan)
			require.Equal(t, "gpt-5.6-luna", plan.SummaryModel())
		}
	}
}

func TestPortableCompactionLegacyUnmappedNonGPTKeepsOwnModel(t *testing.T) {
	for _, model := range []string{"grok-4.7", "deepseek-v4.1-flash"} {
		account := rawChatCompletionsTestAccount()
		account.Extra = map[string]any{"openai_responses_mode": "force_responses"}
		if model == "grok-4.7" {
			account.Platform = PlatformGrok
		}
		cfg := rawChatCompletionsTestConfig()
		cfg.Gateway.OpenAICompactModel = "gpt-5.5"
		upstream := &httpUpstreamRecorder{resp: portableTestHTTPResponse(portableTestResponse("completed"), "application/json")}
		svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
		body := []byte(fmt.Sprintf(`{"model":%q,"input":"save this state"}`, model))
		c, rec := portableTestContext(body, "/v1/responses/compact")
		_, err := svc.Forward(context.Background(), c, account, body)
		require.NoError(t, err, rec.Body.String())
		require.Equal(t, model, gjson.GetBytes(upstream.lastBody, "model").String())
		require.Contains(t, string(upstream.lastBody), portableCompactionPrompt)
	}
}

func TestPortableCompactionBridgeConvertsOnceAndIsolatesScope(t *testing.T) {
	bridge := portableTestBridge()
	account := rawChatCompletionsTestAccount()
	var calls atomic.Int32
	for _, key := range []int64{2, 2, 3} {
		body := []byte(fmt.Sprintf(`{"model":"deepseek-v4.1-flash","input":[{"type":"compaction","encrypted_content":"native-cipher"},{"type":"message","role":"user","content":"NEW_TURN_%d"}]}`, key))
		c, _ := portableTestContext(body, "/v1/responses")
		bridge.Bind(c, func(_ context.Context, _ PortableSummaryScope, request []byte) ([]byte, error) {
			calls.Add(1)
			require.Contains(t, string(request), "native-cipher")
			require.NotContains(t, string(request), "NEW_TURN")
			return []byte(portableTestResponse("completed")), nil
		})
		plan := PlanPortableCompaction(c, account, body)
		require.NotNil(t, plan)
		expanded, done, err := plan.Execute(c, PortableSummaryScope{UserID: 1, APIKeyID: key, GroupID: 10}, body)
		require.NoError(t, err)
		require.False(t, done)
		require.Contains(t, string(expanded), portableTestSummary)
		require.Contains(t, string(expanded), fmt.Sprintf("NEW_TURN_%d", key))
		require.NotContains(t, string(expanded), "native-cipher")
		require.Nil(t, PlanPortableCompaction(c, account, expanded))
	}
	require.EqualValues(t, 2, calls.Load())
}

func TestPortableCompactionBridgePreservesNativeGPTAndRejectsNativeOutput(t *testing.T) {
	bridge := portableTestBridge()
	account := rawChatCompletionsTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"orchestrator-astra": "gpt-6-astra"}
	body := []byte(`{"model":"orchestrator-astra","input":[{"type":"compaction","encrypted_content":"native-cipher"},{"type":"compaction_trigger"}]}`)
	c, _ := portableTestContext(body, "/v1/responses")
	calls := 0
	bridge.Bind(c, func(context.Context, PortableSummaryScope, []byte) ([]byte, error) {
		calls++
		return []byte(`{"status":"completed","output":[{"type":"compaction","encrypted_content":"cipher-again"}]}`), nil
	})
	require.Nil(t, PlanPortableCompaction(c, account, body))
	body = []byte(`{"model":"deepseek-v4.1-flash","input":[{"type":"compaction","encrypted_content":"native-cipher"}]}`)
	for i := 0; i < 2; i++ {
		plan := PlanPortableCompaction(c, account, body)
		_, _, err := plan.Execute(c, PortableSummaryScope{UserID: 1, APIKeyID: 2}, body)
		require.Error(t, err)
	}
	require.Equal(t, 2, calls, "invalid summaries must never enter the cache")
}

func TestPortableCompactionBridgeStrictInferenceKeepsCipherOnRejection(t *testing.T) {
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{"openai_responses_mode": "force_responses"}
	response := portableTestHTTPResponse(`{"error":{"code":"invalid_encrypted_content","message":"invalid checkpoint"}}`, "application/json")
	response.StatusCode = http.StatusBadRequest
	upstream := &httpUpstreamRecorder{resp: response}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	body := []byte(`{"model":"gpt-6-luna","input":[{"type":"compaction","encrypted_content":"native-cipher"},{"type":"message","role":"user","content":"summarize"}]}`)
	c, _ := portableTestContext(body, "/v1/responses")
	ctx := WithPortableSummaryScope(context.Background(), PortableSummaryScope{UserID: 1, APIKeyID: 2})
	c.Request = c.Request.WithContext(ctx)
	_, err := svc.Forward(ctx, c, account, body)
	require.Error(t, err)
	require.Len(t, upstream.bodies, 1)
	require.Contains(t, string(upstream.lastBody), "native-cipher")
	require.True(t, BorrowPortableSummaryUserSlot(ctx, 1, 2))
	require.False(t, BorrowPortableSummaryUserSlot(ctx, 1, 3))
}

type portableBalanceCache struct {
	billingCacheWorkerStub
	balance float64
}

func (c *portableBalanceCache) GetUserBalance(context.Context, int64) (float64, error) {
	return c.balance, nil
}

func TestPortableCompactionBillingRecheckDoesNotIncrementRPM(t *testing.T) {
	cache := &portableBalanceCache{balance: 10}
	rpm := &userRPMCacheStub{userGroupCounts: []int{1, 2}, userCounts: []int{1, 2}}
	svc := &BillingCacheService{cache: cache, cfg: &config.Config{}, userRPMCache: rpm}
	user, group := &User{ID: 1, RPMLimit: 1}, &Group{ID: 10, RPMLimit: 1}
	key := &APIKey{ID: 2}
	require.NoError(t, svc.CheckBillingEligibility(context.Background(), user, key, group, nil, ""))
	require.NoError(t, svc.RecheckBillingEligibility(context.Background(), user, key, group, nil, ""))
	require.EqualValues(t, 1, rpm.userCalls)
	require.EqualValues(t, 1, rpm.userGroupCalls)
	cache.balance = 0
	require.ErrorIs(t, svc.RecheckBillingEligibility(context.Background(), user, key, group, nil, ""), ErrInsufficientBalance)
	cache.balance = 10
	require.ErrorIs(t, svc.CheckBillingEligibility(context.Background(), user, key, group, nil, ""), ErrGroupRPMExceeded)
}

func TestPortableCompactionBridgeCoalescesAndCancelsWaiter(t *testing.T) {
	bridge := portableTestBridge()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	p := &PortableCompactionPlan{model: "gpt-6-luna", binding: portableBridgeBinding{bridge: bridge, run: func(ctx context.Context, _ PortableSummaryScope, _ []byte) ([]byte, error) {
		calls.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return []byte(portableTestResponse("completed")), nil
		}
	}}}
	scope := PortableSummaryScope{UserID: 1, APIKeyID: 2}
	item := map[string]any{"type": "compaction", "encrypted_content": "native"}
	finished := make(chan error, 1)
	go func() { _, err := p.translate(context.Background(), scope, item); finished <- err }()
	<-started
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := p.translate(waitCtx, scope, item)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(release)
	require.NoError(t, <-finished)
	_, err = p.translate(context.Background(), scope, item)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
}

func TestPortableCompactionSummaryCancellationReachesBothTransports(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			account := rawChatCompletionsTestAccount()
			account.Extra = map[string]any{"openai_responses_mode": "force_responses", "openai_passthrough": passthrough}
			upstream := &httpUpstreamRecorder{resp: portableTestHTTPResponse(portableTestResponse("completed"), "application/json")}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			ctx, cancel := context.WithCancel(WithPortableSummaryScope(context.Background(), PortableSummaryScope{UserID: 1, APIKeyID: 2}))
			defer cancel()
			body := []byte(`{"model":"gpt-6-luna","input":[{"type":"message","role":"user","content":"summarize"}]}`)
			c, rec := portableTestContext(body, "/v1/responses")
			c.Request = c.Request.WithContext(ctx)
			_, err := svc.Forward(ctx, c, account, body)
			require.NoError(t, err, rec.Body.String())
			require.NotNil(t, upstream.lastReq)
			require.NoError(t, upstream.lastReq.Context().Err())
			cancel()
			require.ErrorIs(t, upstream.lastReq.Context().Err(), context.Canceled)
		})
	}
}
