package routes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Re-enter the ordinary authenticated gateway so the summary has its own routing,
// account admission, safety checks, pricing, reservation and mandatory usage record.
// No loopback HTTP request, second server, or provider credential is involved.
func portableCompactionBridgeMiddleware(engine http.Handler, bridge *service.PortableCompactionBridge, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := strings.TrimRight(c.Request.URL.Path, "/")
		if c.Request.Method != http.MethodPost || (!strings.HasSuffix(path, "/responses") && !strings.HasSuffix(path, "/responses/compact")) {
			c.Next()
			return
		}
		key, ok := middleware.GetAPIKeyFromContext(c)
		if !ok || key == nil {
			c.Next()
			return
		}
		scope := service.PortableSummaryScope{UserID: key.UserID, APIKeyID: key.ID}
		if key.GroupID != nil {
			scope.GroupID = *key.GroupID
		}
		if service.IsPortableSummaryInference(c.Request.Context()) {
			if !service.PortableSummaryScopeMatches(c.Request.Context(), scope) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"type": "permission_error", "message": "summary authentication scope changed"}})
				return
			}
			c.Next()
			return
		}
		original := c.Request.Clone(c.Request.Context())
		trustForwarded := cfg != nil && cfg.TrustForwardedIPForAPIKeyACL()
		securityClientIP := ip.GetSecurityClientIP(c, trustForwarded)
		bridge.Bind(c, func(parent context.Context, requestedScope service.PortableSummaryScope, body []byte) ([]byte, error) {
			if requestedScope != scope {
				return nil, fmt.Errorf("summary scope does not match parent")
			}
			// Drop all inherited routing/billing/cache IDs while retaining cancellation.
			ctx, cancel := context.WithCancel(context.Background())
			stop := context.AfterFunc(parent, cancel)
			defer stop()
			defer cancel()
			if deadline, ok := parent.Deadline(); ok {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithDeadline(ctx, deadline)
				defer deadlineCancel()
			}
			if err := parent.Err(); err != nil {
				return nil, err
			}
			ctx = service.WithPortableSummaryScope(ctx, scope)
			req := original.Clone(ctx)
			if securityClientIP != "" {
				req.RemoteAddr = net.JoinHostPort(securityClientIP, "0")
			}
			req.Method = http.MethodPost
			req.URL.Path = "/v1/responses"
			req.URL.RawPath = ""
			req.URL.RawQuery = ""
			req.RequestURI = "/v1/responses"
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.GetBody = nil
			req.ContentLength = int64(len(body))
			req.TransferEncoding = nil
			// Preserve authentication and client identification, never old transport,
			// session affinity or parent request IDs. Capability advertisement is
			// retained for Codex client admission; only the body can request compaction.
			req.Header = make(http.Header)
			for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "User-Agent", "Originator", "OpenAI-Version", "X-Codex-Beta-Features", "X-Codex-Client-Version"} {
				if value := original.Header.Get(name); value != "" {
					req.Header.Set(name, value)
				}
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			capture := &portableSummaryHTTPWriter{header: make(http.Header), cancel: cancel}
			engine.ServeHTTP(capture, req)
			if capture.err != nil {
				return nil, capture.err
			}
			if err := parent.Err(); err != nil {
				return nil, err
			}
			if capture.status < 200 || capture.status >= 300 {
				return nil, fmt.Errorf("plaintext summary request failed (HTTP %d): %s", capture.status, strings.TrimSpace(string(capture.body.Bytes())))
			}
			return append([]byte(nil), capture.body.Bytes()...), nil
		})
		c.Next()
	}
}

type portableSummaryHTTPWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	err    error
	cancel context.CancelFunc
}

func (w *portableSummaryHTTPWriter) Header() http.Header { return w.header }
func (w *portableSummaryHTTPWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *portableSummaryHTTPWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.body.Len()+len(data) > 2*1024*1024 {
		w.err = fmt.Errorf("plaintext summary response exceeds capture limit")
		w.cancel()
		return 0, w.err
	}
	return w.body.Write(data)
}
func (w *portableSummaryHTTPWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}
