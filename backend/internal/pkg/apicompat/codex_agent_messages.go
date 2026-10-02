// SPDX-License-Identifier: MIT
// Adapted by copying the two message-normalization functions unchanged from
// router-for-me/CLIProxyAPI commit 2eb8dd11d2480c5fd8bc8f2796cec6af534bc3b6.
// Copyright (c) Router-For.ME and contributors. See CPA-MIT-LICENSE.
package apicompat

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func rewriteCodexAgentMessageInput(payload []byte) []byte {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload
	}

	updated := rewriteCodexAgentMessageContent(payload)
	for itemIndex, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "agent_message" {
			continue
		}
		itemPath := fmt.Sprintf("input.%d", itemIndex)
		var errSet error
		updated, errSet = sjson.SetBytes(updated, itemPath+".role", "user")
		if errSet != nil {
			return payload
		}
		updated, errSet = sjson.SetBytes(updated, itemPath+".type", "message")
		if errSet != nil {
			return payload
		}
	}
	return updated
}

func rewriteCodexAgentMessageContent(payload []byte) []byte {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload
	}

	updated := payload
	for itemIndex, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "agent_message" {
			continue
		}
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		for partIndex, part := range content.Array() {
			if strings.TrimSpace(part.Get("type").String()) != "encrypted_content" {
				continue
			}
			encryptedContent := part.Get("encrypted_content")
			if encryptedContent.Type != gjson.String {
				continue
			}
			partPath := fmt.Sprintf("input.%d.content.%d", itemIndex, partIndex)
			var errSet error
			updated, errSet = sjson.SetBytes(updated, partPath+".type", "input_text")
			if errSet != nil {
				return payload
			}
			updated, errSet = sjson.SetBytes(updated, partPath+".text", encryptedContent.String())
			if errSet != nil {
				return payload
			}
			updated, errSet = sjson.DeleteBytes(updated, partPath+".encrypted_content")
			if errSet != nil {
				return payload
			}
		}
	}
	return updated
}

// NormalizeCodexAgentMessagesForAntigravity makes Codex delegation content
// portable before Responses -> Anthropic -> Gemini conversion. It changes only
// agent_message items; the original request bytes remain available to billing.
func NormalizeCodexAgentMessagesForAntigravity(payload []byte) []byte {
	return rewriteCodexAgentMessageInput(payload)
}

// NormalizeCodexAgentMessagesForResponses makes Codex delegation items portable
// for Responses providers that do not understand agent_message. Only the
// delegation envelope is rewritten; encrypted reasoning items remain untouched.
func NormalizeCodexAgentMessagesForResponses(payload []byte) []byte {
	return rewriteCodexAgentMessageInput(payload)
}
