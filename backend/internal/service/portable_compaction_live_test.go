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

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Opt-in only: patched service -> local gateway -> real Grok. Never uses fake outputs.
type portableLiveUpstream struct {
	t      *testing.T
	client *http.Client
	dir    string
	calls  int
}

func (u *portableLiveUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if req.URL.Scheme != "http" || req.URL.Host != "127.0.0.1:8320" || req.URL.Path != "/v1/responses" {
		return nil, fmt.Errorf("live probe only permits the local gateway")
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	require.False(u.t, gjson.GetBytes(body, "background").Exists())
	require.NotContains(u.t, string(body), "compaction_trigger")
	require.NotContains(u.t, string(body), "encrypted_content")
	u.calls++
	require.NoError(u.t, os.WriteFile(filepath.Join(u.dir, fmt.Sprintf("upstream-%d-request.json", u.calls)), body, 0600))
	req.Body = io.NopCloser(bytes.NewReader(body))
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	require.NoError(u.t, os.WriteFile(filepath.Join(u.dir, fmt.Sprintf("upstream-%d-response.txt", u.calls)), raw, 0600))
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}

func (u *portableLiveUpstream) DoWithTLS(req *http.Request, proxy string, account int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, account, concurrency)
}

func TestPortableCompactionLiveGrok(t *testing.T) {
	key := os.Getenv("LIVE_GROK_GATEWAY_TOKEN")
	if key == "" {
		t.Skip("requires explicit live Grok test credential")
	}
	dir := os.Getenv("LIVE_GROK_EVIDENCE_DIR")
	require.NotEmpty(t, dir)
	require.NoError(t, os.MkdirAll(dir, 0700))
	upstream := &portableLiveUpstream{t: t, client: &http.Client{Timeout: 240 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, dir: dir}
	account := rawChatCompletionsTestAccount()
	account.Platform = PlatformGrok
	account.Credentials["api_key"] = key
	account.Credentials["base_url"] = "http://127.0.0.1:8320/v1"
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	nonce := uuid.NewString()
	expected := map[string]string{"project": "Otter-" + nonce, "file": "F:/fixture/" + nonce + "/relay.toml", "port": "18437", "checkpoint": "CHECKPOINT_" + nonce, "completed": "THEME_EDIT_ONLY", "next": "VERIFY_PORT_THEN_WAIT_FOR_APPROVAL"}
	facts, err := json.Marshal(expected)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{"model": "grok-4.7", "stream": false, "store": false, "background": true, "input": []any{
		map[string]any{"type": "message", "role": "user", "content": "Preserve all six literal state values for the next turn."},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": string(facts)}}},
		map[string]any{"type": "compaction_trigger"},
	}})
	require.NoError(t, err)
	c, rec := portableTestContext(body, "/v1/responses")
	_, err = svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err, rec.Body.String())
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "compact-response.json"), rec.Body.Bytes(), 0600))
	packet := gjson.Parse(rec.Body.String()).Get("output.0.encrypted_content").String()
	envelope, err := decodePortableCompaction(packet)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(envelope.Summary), 0600))
	require.Contains(t, envelope.Summary, expected["checkpoint"])
	body, err = json.Marshal(map[string]any{"model": "grok-4.7", "stream": false, "store": false, "input": []any{
		map[string]any{"type": "compaction", "encrypted_content": packet},
		map[string]any{"type": "message", "role": "user", "content": "Return ONLY a JSON object with the six saved fields: project, file, port (string), checkpoint, completed, next. Use the exact saved values."},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "replay-request.json"), body, 0600))
	c, rec = portableTestContext(body, "/v1/responses")
	_, err = svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err, rec.Body.String())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "replay-response.json"), rec.Body.Bytes(), 0600))
	var text []string
	gjson.Parse(rec.Body.String()).Get("output").ForEach(func(_, item gjson.Result) bool {
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
	require.Equal(t, 2, upstream.calls)
	report, err := json.MarshalIndent(map[string]any{"passed": true, "model": "grok-4.7", "real_model_calls": upstream.calls, "fact_count": len(expected), "bridge": "patched service -> running local gateway -> real Grok", "expected": expected, "recovered": got}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "report.json"), report, 0600))
}
