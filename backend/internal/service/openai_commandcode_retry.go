package service

import (
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const commandCodeTransient400MaxRetries = 2

// Identical real requests can receive this opaque Command Code error and then
// succeed unchanged. Do not retry specific validation errors or other hosts.
func isCommandCodeTransient400(account *Account, status int, body []byte) bool {
	if status != http.StatusBadRequest || account == nil || !account.IsOpenAIApiKey() ||
		!isOfficialCommandCodeHost(account.GetOpenAIBaseURL()) || !gjson.ValidBytes(body) {
		return false
	}
	err := gjson.ParseBytes(body)
	if wrapped := err.Get("error"); wrapped.IsObject() {
		err = wrapped
	}
	for depth := 0; depth < 2; depth++ {
		if err.Get("type").String() != "invalid_request_error" ||
			strings.TrimSpace(err.Get("code").String()) != "" ||
			strings.TrimSpace(err.Get("param").String()) != "" {
			return false
		}
		message := strings.TrimSpace(err.Get("message").String())
		if depth == 0 && gjson.Valid(message) {
			err = gjson.Parse(message)
			continue
		}
		const prefix = "invalid request error trace_id: "
		if !strings.HasPrefix(message, prefix) {
			return false
		}
		trace := strings.TrimPrefix(message, prefix)
		if len(trace) != 32 {
			return false
		}
		_, decodeErr := hex.DecodeString(trace)
		return decodeErr == nil
	}
	return false
}
