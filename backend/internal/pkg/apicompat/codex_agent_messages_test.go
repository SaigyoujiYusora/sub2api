package apicompat_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

func convertDelegationToGemini(t *testing.T, body []byte) []byte {
	t.Helper()
	var req apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	anthropic, err := apicompat.ResponsesToAnthropicRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(anthropic)
	if err != nil {
		t.Fatal(err)
	}
	var claude antigravity.ClaudeRequest
	if err = json.Unmarshal(raw, &claude); err != nil {
		t.Fatal(err)
	}
	gemini, err := antigravity.TransformClaudeToGeminiWithOptions(&claude, "test-project", "gemini-3.8-flash-high", antigravity.TransformOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return gemini
}

func TestCodexAgentPortableFirstAndFollowupReachGemini(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash-high","stream":true,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"环境信息"}]},
		{"type":"agent_message","id":"amsg_first","author":"/root","recipient":"/root/designer","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},{"type":"encrypted_content","encrypted_content":"FIRST_715 记住口令杉叶，计算17*19"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"323"}]},
		{"type":"agent_message","id":"amsg_followup","author":"/root","recipient":"/root/designer","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},{"type":"encrypted_content","encrypted_content":"SECOND_826 沿用首轮口令，把结果加11"}]}
	]}`)
	original := append([]byte(nil), body...)
	before := convertDelegationToGemini(t, body)
	if bytes.Contains(before, []byte("FIRST_715")) || bytes.Contains(before, []byte("SECOND_826")) {
		t.Fatal("unpatched fixture should reproduce missing task bodies")
	}
	fixed := apicompat.NormalizeCodexAgentMessagesForAntigravity(body)
	if !bytes.Equal(body, original) {
		t.Fatal("mutated original request buffer")
	}
	after := convertDelegationToGemini(t, fixed)
	for _, marker := range []string{"FIRST_715", "SECOND_826", "记住口令杉叶", "沿用首轮口令"} {
		if !bytes.Contains(after, []byte(marker)) {
			t.Fatalf("Gemini did not receive %q: %s", marker, after)
		}
	}
	if bytes.Contains(after, []byte("encrypted_content")) {
		t.Fatal("Responses-only content block leaked to Gemini")
	}
	if !bytes.Equal(apicompat.NormalizeCodexAgentMessagesForAntigravity(fixed), fixed) {
		t.Fatal("normalization must be idempotent")
	}
}

func TestCodexAgentPortablePreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash-high","seed":9007199254740993123,"metadata":{"encrypted_content":"untouched"},"input":[{"type":"reasoning","encrypted_content":"gAAAA_original_reasoning"},{"type":"function_call_output","call_id":"call1","output":{"encrypted_content":"unchanged_tool_result"}},{"type":"agent_message","id":"amsg1","author":"/root","recipient":"/root/a","content":[{"type":"input_text","text":"Payload:"},{"type":"encrypted_content","encrypted_content":"TASK"},{"type":"encrypted_content","encrypted_content":42}]}]}`)
	fixed := apicompat.NormalizeCodexAgentMessagesForAntigravity(body)
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal(body, &before)
	_ = json.Unmarshal(fixed, &after)
	for _, key := range []string{"model", "seed", "metadata"} {
		if !bytes.Equal(before[key], after[key]) {
			t.Fatal("unrelated field changed:", key)
		}
	}
	var oldItems, newItems []json.RawMessage
	_ = json.Unmarshal(before["input"], &oldItems)
	_ = json.Unmarshal(after["input"], &newItems)
	for i := 0; i < 2; i++ {
		if !bytes.Equal(oldItems[i], newItems[i]) {
			t.Fatal("reasoning/tool result changed")
		}
	}
	var item map[string]json.RawMessage
	_ = json.Unmarshal(newItems[2], &item)
	if string(item["type"]) != `"message"` || string(item["role"]) != `"user"` || string(item["id"]) != `"amsg1"` || string(item["author"]) != `"/root"` {
		t.Fatal(string(newItems[2]))
	}
	if !strings.Contains(string(item["content"]), `"text":"TASK"`) || !strings.Contains(string(item["content"]), `"encrypted_content":42`) {
		t.Fatal(string(item["content"]))
	}
}

func TestCodexAgentPortableOrdinaryRequestsUnchanged(t *testing.T) {
	for _, body := range []string{`{"model":"gemini-3.8-flash-high","input":"hi"}`, ` { "model":"gemini-3.8-flash-high", "input":[{"type":"message","role":"user","content":"hello"},{"type":"reasoning","encrypted_content":"gAAAA"}] } `, `{"input":null}`, `{"input":{"type":"agent_message"}}`} {
		if out := apicompat.NormalizeCodexAgentMessagesForAntigravity([]byte(body)); string(out) != body {
			t.Fatalf("changed unrelated request: %s", out)
		}
	}
}
