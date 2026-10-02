package routes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPortableCompactionBridgeReauthenticatesWithCleanContextAndTrustedIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	require.NoError(t, router.SetTrustedProxies([]string{"10.0.0.0/8"}))
	cfg := &config.Config{}
	cfg.Gateway.PortableSummaryModelMapping = map[string]string{"grok-*": "gpt-6-luna"}
	bridge := service.NewPortableCompactionBridge(cfg)
	scope := service.PortableSummaryScope{UserID: 1, APIKeyID: 2, GroupID: 10}
	var authenticated atomic.Int32
	router.Use(func(c *gin.Context) {
		authenticated.Add(1)
		require.Equal(t, "Bearer local-test", c.GetHeader("Authorization"))
		require.Equal(t, "203.0.113.17", ip.GetSecurityClientIP(c, false), "the reauthenticated request must retain the trusted client IP")
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 1, GroupID: &scope.GroupID, Group: &service.Group{ID: 10, Platform: service.PlatformComposite}})
		c.Next()
	})
	router.Use(portableCompactionBridgeMiddleware(router, bridge, cfg))
	var activeUserSlots atomic.Int32
	router.POST("/v1/responses", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		if service.IsPortableSummaryInference(c.Request.Context()) {
			require.True(t, service.BorrowPortableSummaryUserSlot(c.Request.Context(), 1, 2))
			require.EqualValues(t, 1, activeUserSlots.Load(), "nested inference borrows the held user slot")
			require.Nil(t, c.Request.Context().Value("outer-only"))
			require.Empty(t, c.GetHeader("x-client-request-id"))
			require.Empty(t, c.GetHeader("session_id"))
			require.Equal(t, "remote_compaction_v2", c.GetHeader("x-codex-beta-features"))
			require.Equal(t, "gpt-6-luna", gjson.GetBytes(body, "model").String())
			require.NotContains(t, string(body), "compaction_trigger")
			c.Data(200, "application/json", []byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"saved task state"}]}],"usage":{"input_tokens":4,"output_tokens":3}}`))
			return
		}
		require.True(t, activeUserSlots.CompareAndSwap(0, 1))
		defer activeUserSlots.Store(0)
		plan := service.PlanPortableCompaction(c, &service.Account{Platform: service.PlatformGrok}, body)
		require.NotNil(t, plan)
		_, done, err := plan.Execute(c, scope, body)
		require.NoError(t, err)
		require.True(t, done)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4.7","stream":false,"input":[{"type":"message","role":"user","content":"remember"},{"type":"compaction_trigger"}]}`))
	req = req.WithContext(context.WithValue(req.Context(), "outer-only", true))
	req.RemoteAddr = "10.0.0.2:8123"
	req.Header.Set("X-Forwarded-For", "203.0.113.17")
	req.Header.Set("Authorization", "Bearer local-test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-client-request-id", "parent-id")
	req.Header.Set("session_id", "parent-session")
	req.Header.Set("x-codex-beta-features", "remote_compaction_v2")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.EqualValues(t, 2, authenticated.Load())
	require.Zero(t, activeUserSlots.Load())
	require.Contains(t, rec.Body.String(), "sub2api:compact:v1:")
}

func TestPortableCompactionBridgeBoundedCapture(t *testing.T) {
	canceled := false
	w := &portableSummaryHTTPWriter{header: make(http.Header), cancel: func() { canceled = true }}
	_, err := w.Write(make([]byte, 2*1024*1024+1))
	require.Error(t, err)
	require.True(t, canceled)
	require.Zero(t, w.body.Len())
}
