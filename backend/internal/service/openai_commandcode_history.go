package service

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// dropCommandCodeUnansweredToolCalls mirrors the Chat bridge's treatment of
// calls interrupted before a result was recorded. Command Code rejects an
// otherwise valid Responses history if even one such call remains. Keep all
// recorded results and ordinary messages, and never invent a successful result.
func dropCommandCodeUnansweredToolCalls(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	root := gjson.ParseBytes(body)
	if strings.TrimSpace(root.Get("previous_response_id").String()) != "" ||
		(root.Get("conversation").Exists() && root.Get("conversation").Type != gjson.Null) {
		return body // Results can be held in upstream state.
	}
	input := root.Get("input")
	if !input.IsArray() {
		return body
	}
	items := input.Array()
	outputs := make(map[string]struct{})
	for _, item := range items {
		typ := item.Get("type").String()
		if typ == "item_reference" {
			return body // A referenced server item may supply the missing result.
		}
		if typ == "function_call_output" || typ == "custom_tool_call_output" {
			if id := item.Get("call_id").String(); id != "" {
				outputs[typ+":"+id] = struct{}{}
			}
		}
	}
	kept := make([]string, 0, len(items))
	for _, item := range items {
		typ, id := item.Get("type").String(), item.Get("call_id").String()
		if (typ == "function_call" || typ == "custom_tool_call") && id != "" {
			if _, ok := outputs[typ+"_output:"+id]; !ok {
				continue
			}
		}
		kept = append(kept, item.Raw)
	}
	if len(kept) == len(items) {
		return body
	}
	normalized, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(kept, ",")+"]"))
	if err != nil {
		return body
	}
	return normalized
}
