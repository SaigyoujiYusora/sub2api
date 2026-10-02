package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// PortableSummaryScope binds the nested inference to the authenticated parent's scope.
type PortableSummaryScope struct{ UserID, APIKeyID, GroupID int64 }
type portableSummaryScopeKey struct{}

func WithPortableSummaryScope(ctx context.Context, scope PortableSummaryScope) context.Context {
	return context.WithValue(ctx, portableSummaryScopeKey{}, scope)
}

func IsPortableSummaryInference(ctx context.Context) bool {
	_, ok := ctx.Value(portableSummaryScopeKey{}).(PortableSummaryScope)
	return ok
}

func PortableSummaryScopeMatches(ctx context.Context, scope PortableSummaryScope) bool {
	expected, ok := ctx.Value(portableSummaryScopeKey{}).(PortableSummaryScope)
	return ok && expected == scope
}

// BorrowPortableSummaryUserSlot only borrows the authenticated parent's held user slot.
// The parent releases its account slot and budget before invoking the nested handler.
func BorrowPortableSummaryUserSlot(ctx context.Context, userID, apiKeyID int64) bool {
	scope, ok := ctx.Value(portableSummaryScopeKey{}).(PortableSummaryScope)
	return ok && scope.UserID == userID && scope.APIKeyID == apiKeyID && userID > 0 && apiKeyID > 0
}

type PortableSummaryRunner func(context.Context, PortableSummaryScope, []byte) ([]byte, error)
type portableBridgeBinding struct {
	bridge *PortableCompactionBridge
	run    PortableSummaryRunner
}

const portableBridgeBindingKey = "portable_compaction_bridge"

type portableCachedSummary struct {
	packet  map[string]any
	expires time.Time
}
type portableSummaryFlight struct {
	done   chan struct{}
	packet map[string]any
	err    error
}

// PortableCompactionBridge caches only validated checkpoint translations. Cache entries
// are isolated by user/key/group, packet digest, model, and prompt version, and bounded.
// In-memory entries expire after 24h; a restart safely regenerates a missing entry.
type PortableCompactionBridge struct {
	cfg     *config.Config
	mu      sync.Mutex
	cache   map[string]portableCachedSummary
	flights map[string]*portableSummaryFlight
}

func NewPortableCompactionBridge(cfg *config.Config) *PortableCompactionBridge {
	return &PortableCompactionBridge{cfg: cfg, cache: make(map[string]portableCachedSummary), flights: make(map[string]*portableSummaryFlight)}
}

func (b *PortableCompactionBridge) Bind(c *gin.Context, run PortableSummaryRunner) {
	c.Set(portableBridgeBindingKey, portableBridgeBinding{b, run})
}

type PortableCompactionPlan struct {
	binding portableBridgeBinding
	model   string
	compact bool
}

// PlanPortableCompaction must run after selecting the actual target account, but
// before forwarding. A caller must release selected account slots before Execute.
func PlanPortableCompaction(c *gin.Context, account *Account, body []byte) *PortableCompactionPlan {
	value, ok := c.Get(portableBridgeBindingKey)
	binding, valid := value.(portableBridgeBinding)
	if !ok || !valid || binding.bridge.cfg == nil || binding.run == nil || IsPortableSummaryInference(c.Request.Context()) {
		return nil
	}
	requested := gjson.GetBytes(body, "model").String()
	resolved := resolveOpenAIAccountUpstreamModelForRequest(account, requested, false)
	if UsesNativeGPTCompaction(account, resolved) || isOpenAIImageModel(resolved) {
		return nil
	}
	if isPortableCompactionRequest(c, body) {
		model, matched := account.ResolveCompactMappedModel(requested)
		if !matched {
			model, matched = account.ResolveCompactMappedModel(resolved)
		}
		if !matched {
			model, matched = resolveRequestedModelInMapping(binding.bridge.cfg.Gateway.PortableSummaryModelMapping, requested)
		}
		if !matched {
			model, matched = resolveRequestedModelInMapping(binding.bridge.cfg.Gateway.PortableSummaryModelMapping, resolved)
		}
		if matched && strings.TrimSpace(model) != "" {
			return &PortableCompactionPlan{binding: binding, model: strings.TrimSpace(model), compact: true}
		}
	}
	if strings.TrimSpace(binding.bridge.cfg.Gateway.PortableConversionModel) == "" || !hasNativeCompactionInputs(body) {
		return nil
	}
	return &PortableCompactionPlan{binding: binding, model: strings.TrimSpace(binding.bridge.cfg.Gateway.PortableConversionModel)}
}

func hasNativeCompactionInputs(body []byte) bool {
	found := false
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		kind := item.Get("type").String()
		found = (kind == "compaction" || kind == "context_compaction" || kind == "compaction_summary") && !strings.HasPrefix(item.Get("encrypted_content").String(), portableCompactionPrefix)
		return !found
	})
	return found
}

func (p *PortableCompactionPlan) summary(ctx context.Context, scope PortableSummaryScope, body []byte) ([]byte, error) {
	summary, err := buildPortableSummaryRequest(body)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err = decodeOpenAIJSONUseNumber(summary, &payload); err != nil {
		return nil, err
	}
	payload["model"] = p.model
	payload["service_tier"] = "default"
	instructions, _ := payload["instructions"].(string)
	payload["instructions"] = strings.TrimSpace(instructions + "\n\n" + portableCompactionPrompt)
	// These identify the original inference rather than this separate summary call.
	delete(payload, "prompt_cache_key")
	delete(payload, "metadata")
	delete(payload, "client_metadata")
	summary, err = marshalOpenAIUpstreamJSON(payload)
	if err != nil {
		return nil, err
	}
	response, err := p.binding.run(ctx, scope, summary)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	// Reject native compaction, refusals, partial outputs and tool calls. Only a
	// completed ordinary text answer can become a portable checkpoint.
	return buildPortableCompactionResponse(response, p.model)
}

func (p *PortableCompactionPlan) translate(ctx context.Context, scope PortableSummaryScope, item map[string]any) (map[string]any, error) {
	content, _ := item["encrypted_content"].(string)
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("native checkpoint is empty")
	}
	digest := sha256.Sum256([]byte(content))
	key := fmt.Sprintf("v1:%d:%d:%d:%s:%s", scope.UserID, scope.APIKeyID, scope.GroupID, p.model, hex.EncodeToString(digest[:]))
	b := p.binding.bridge
	b.mu.Lock()
	if cached, ok := b.cache[key]; ok && time.Now().Before(cached.expires) {
		b.mu.Unlock()
		return cached.packet, nil
	}
	if flight, ok := b.flights[key]; ok {
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
			return flight.packet, flight.err
		}
	}
	flight := &portableSummaryFlight{done: make(chan struct{})}
	b.flights[key] = flight
	b.mu.Unlock()
	// No post-checkpoint turns or current user request enter the cached summary.
	seed, err := marshalOpenAIUpstreamJSON(map[string]any{"model": p.model, "input": []any{item}})
	var packet map[string]any
	if err == nil {
		var response []byte
		response, err = p.summary(ctx, scope, seed)
		if err == nil {
			err = json.Unmarshal([]byte(gjson.GetBytes(response, "output.0").Raw), &packet)
		}
	}
	b.mu.Lock()
	if err == nil {
		if len(b.cache) >= 1024 {
			for k := range b.cache {
				delete(b.cache, k)
				break
			}
		}
		b.cache[key] = portableCachedSummary{packet: packet, expires: time.Now().Add(24 * time.Hour)}
	}
	flight.packet, flight.err = packet, err
	delete(b.flights, key)
	close(flight.done)
	b.mu.Unlock()
	return packet, err
}

// Execute returns a rewritten ordinary request or emits the completed compaction.
// The authenticated normal inference path owns all billing for the nested call.
func (p *PortableCompactionPlan) Execute(c *gin.Context, scope PortableSummaryScope, body []byte) ([]byte, bool, error) {
	ctx := c.Request.Context()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		return nil, false, fmt.Errorf("portable conversion requires complete stateless history")
	}
	// Recheck summary-model access even on a cache hit; the inner HTTP chain checks
	// this again on a miss along with authentication, routing, budget and safety.
	if p.compact {
		// A full explicit compaction can summarize native checkpoints and newer
		// messages together; it does not need individual cached translations.
		expanded, _, err := ExpandPortableCompactionInputs(body, true)
		if err != nil {
			return nil, false, err
		}
		response, err := p.summary(ctx, scope, expanded)
		if err != nil {
			return nil, false, err
		}
		if gjson.GetBytes(body, "stream").Bool() || openAICompactClientWantsStream(c) {
			MarkOpenAICompactClientStream(c)
			if !writeOpenAICompactSSEBridge(c, http.StatusOK, response) {
				return nil, false, fmt.Errorf("could not encode portable summary response")
			}
		} else {
			StopOpenAICompactSSEKeepaliveCommitted(c)
			c.Data(http.StatusOK, "application/json", response)
		}
		return nil, true, nil
	}
	var payload map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &payload); err != nil {
		return nil, false, err
	}
	items, _ := payload["input"].([]any)
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := item["type"].(string)
		content, _ := item["encrypted_content"].(string)
		if (kind != "compaction" && kind != "context_compaction" && kind != "compaction_summary") || strings.HasPrefix(content, portableCompactionPrefix) {
			continue
		}
		packet, err := p.translate(ctx, scope, item)
		if err != nil {
			return nil, false, err
		}
		items[i] = packet
	}
	payload["input"] = items
	encoded, err := marshalOpenAIUpstreamJSON(payload)
	if err != nil {
		return nil, false, err
	}
	expanded, _, err := ExpandPortableCompactionInputs(encoded, false)
	return expanded, false, err
}

func WritePortableCompactionError(c *gin.Context, err error) {
	writePortableCompactionError(c, http.StatusBadGateway, err)
}

func (p *PortableCompactionPlan) SummaryModel() string { return p.model }
