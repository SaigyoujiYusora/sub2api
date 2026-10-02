//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Opt-in integration probe. Candidate bridge logic runs in-process; all model
// inference passes through the already-running gateway, with real usage billing.
func TestPortableCompactionBridgeLiveLuna(t *testing.T) {
	key := os.Getenv("LIVE_GROK_GATEWAY_TOKEN")
	if key == "" {
		t.Skip("requires explicit live gateway credential")
	}
	dir := os.Getenv("LIVE_GROK_EVIDENCE_DIR")
	require.NotEmpty(t, dir)
	require.NoError(t, os.MkdirAll(dir, 0700))
	client := &http.Client{Timeout: 240 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	calls := 0
	var observations []map[string]any
	call := func(ctx context.Context, label string, body []byte) ([]byte, error) {
		calls++
		name := fmt.Sprintf("%02d-%s", calls, label)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name+"-request.json"), body, 0600))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:8320/v1/responses", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "codex-cli/0.159.2")
		req.Header.Set("originator", "codex_cli_rs")
		req.Header.Set("x-codex-beta-features", "remote_compaction_v2")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
		if err != nil {
			return nil, err
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, name+"-response.txt"), raw, 0600))
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s HTTP %d: %s", label, resp.StatusCode, raw)
		}
		final := raw
		if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			final = nil
			for _, line := range strings.Split(string(raw), "\n") {
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				if event.Get("type").String() == "response.completed" {
					final = []byte(event.Get("response").Raw)
				}
			}
		}
		if gjson.GetBytes(final, "status").String() != "completed" {
			return nil, fmt.Errorf("%s did not return a completed response", label)
		}
		observations = append(observations, map[string]any{"label": label, "requested_model": gjson.GetBytes(body, "model").String(), "response_model": gjson.GetBytes(final, "model").String(), "usage": gjson.GetBytes(final, "usage").Value(), "request_id": resp.Header.Get("x-request-id")})
		return final, nil
	}
	bridge := portableTestBridge()
	scope := PortableSummaryScope{UserID: 1, APIKeyID: 2, GroupID: 10}
	runner := func(ctx context.Context, _ PortableSummaryScope, body []byte) ([]byte, error) {
		require.Equal(t, "gpt-6-luna", gjson.GetBytes(body, "model").String())
		require.False(t, HasCompactionTriggerInInput(body))
		require.False(t, gjson.GetBytes(body, "background").Exists())
		require.False(t, gjson.GetBytes(body, "context_management").Exists())
		return call(ctx, "luna-plaintext-summary", body)
	}
	nonce := uuid.NewString()
	expected := map[string]string{"project": "Copper-" + nonce, "file": "F:/fixture/" + nonce + "/relay.toml", "port": "18437", "checkpoint": "CHECKPOINT_" + nonce, "completed": "THEME_EDIT_ONLY", "next": "VERIFY_PORT_THEN_WAIT_FOR_APPROVAL"}
	facts, _ := json.Marshal(expected)
	fixture := []any{map[string]any{"type": "message", "role": "user", "content": "Preserve every literal field from the assistant's task state."}, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": string(facts)}}}, map[string]any{"type": "compaction_trigger"}}
	marshal := func(v any) []byte { b, err := json.Marshal(v); require.NoError(t, err); return b }
	recoverFacts := func(body []byte) {
		response, err := call(context.Background(), "deepseek-readback", body)
		require.NoError(t, err)
		var text []string
		gjson.GetBytes(response, "output").ForEach(func(_, item gjson.Result) bool {
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "output_text" {
					text = append(text, part.Get("text").String())
				}
				return true
			})
			return true
		})
		answer := strings.Join(text, "\n")
		start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
		require.GreaterOrEqual(t, start, 0, answer)
		require.Greater(t, end, start, answer)
		var got map[string]string
		require.NoError(t, json.Unmarshal([]byte(answer[start:end+1]), &got))
		require.Equal(t, expected, got)
	}
	question := map[string]any{"type": "message", "role": "user", "content": "Return ONLY JSON with the six saved fields: project, file, port (string), checkpoint, completed, next. Use exact saved values."}
	// A DS compaction is fulfilled by Luna ordinary inference, not a DS call or
	// native GPT compact call. DS then consumes only the portable checkpoint.
	body := marshal(map[string]any{"model": "deepseek-v4.1-flash", "stream": false, "input": fixture})
	c, rec := portableTestContext(body, "/v1/responses")
	bridge.Bind(c, runner)
	account := rawChatCompletionsTestAccount()
	plan := PlanPortableCompaction(c, account, body)
	require.NotNil(t, plan)
	_, done, err := plan.Execute(c, scope, body)
	require.NoError(t, err)
	require.True(t, done)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ds-luna-compaction.json"), rec.Body.Bytes(), 0600))
	packet := gjson.Get(rec.Body.String(), "output.0")
	envelope, err := decodePortableCompaction(packet.Get("encrypted_content").String())
	require.NoError(t, err)
	require.Equal(t, "gpt-6-luna", envelope.Model)
	recoverFacts(marshal(map[string]any{"model": "deepseek-v4.1-flash", "stream": false, "input": []any{packet.Value(), question}}))
	// Produce a real Astra native checkpoint, then translate it on demand for DS.
	native, err := call(context.Background(), "astra-native-compact", marshal(map[string]any{"model": "gpt-6-astra", "stream": true, "store": false, "input": fixture}))
	require.NoError(t, err)
	var nativeItem any
	gjson.GetBytes(native, "output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "compaction" {
			nativeItem = item.Value()
			return false
		}
		return true
	})
	require.NotNil(t, nativeItem)
	nativeBody := marshal(map[string]any{"model": "deepseek-v4.1-flash", "stream": false, "input": []any{nativeItem, question}})
	c, _ = portableTestContext(nativeBody, "/v1/responses")
	bridge.Bind(c, runner)
	plan = PlanPortableCompaction(c, account, nativeBody)
	require.NotNil(t, plan)
	expanded, done, err := plan.Execute(c, scope, nativeBody)
	require.NoError(t, err)
	require.False(t, done)
	require.NotContains(t, string(expanded), "encrypted_content")
	recoverFacts(expanded)
	before := calls
	_, _, err = plan.Execute(c, scope, nativeBody)
	require.NoError(t, err)
	require.Equal(t, before, calls, "same checkpoint must reuse the summary cache")
	report := marshal(map[string]any{"passed": true, "cases": []string{"DS trigger -> Luna ordinary plaintext -> DS readback", "Astra native -> Luna plaintext -> DS readback", "cached translation makes no model request"}, "facts_recovered": 6, "real_model_calls": calls, "requests": observations, "scope": "candidate coordinator with real existing gateway; no service deployment"})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "report.json"), report, 0600))
}
