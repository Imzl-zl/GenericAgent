package api

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ctxKey is the unexported context key type for HTTP middleware values.
type ctxKey int

const ctxUserIDKey ctxKey = 0

// auth wraps a handler with the platform admin token check. The admin token is a
// single shared secret used by smoke tooling and the loopback admin path; it
// is NOT a user authentication mechanism. User-authenticated endpoints use
// userAuth instead.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Platform-Admin-Token")
		if tok == "" || tok != s.adminToken {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid X-Platform-Admin-Token", traceID())
			return
		}
		next(w, r)
	}
}

// userAuth wraps a handler with Bearer-token session validation via the
// InviteService. On success, the authenticated user id is stored in the
// request context under ctxUserIDKey.
func (s *Server) userAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.invite == nil {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "user authentication not configured", traceID())
			return
		}
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing Authorization header", traceID())
			return
		}
		token := strings.TrimSpace(header[len(prefix):])
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "empty bearer token", traceID())
			return
		}
		userID, err := s.invite.ValidateSession(r.Context(), token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error(), traceID())
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUserIDKey, userID)))
	}
}

// userIDFromContext extracts the user id set by userAuth. Returns false when
// no user is authenticated (admin token path).
func userIDFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(ctxUserIDKey).(int64)
	return v, ok
}

// ServeContext runs the HTTP server with sane timeouts and middleware until
// ctx is cancelled. The handler chain applies: body-size limit (prevents
// memory exhaustion), panic recovery (logs stack trace, returns 500 without
// leaking internals). Timeouts protect against Slowloris and stuck writers.
// 主 API 只允许 loopback(审查: 管理/用户 API 不外泄到容器网络)。
func ServeContext(ctx context.Context, addr string, h http.Handler) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("platform API must bind loopback, got %s", addr)
	}
	return serveUntil(ctx, addr, h, DefaultMaxRequestBodyBytes)
}

// ServeInternalContext 运行显式启用的内部 listener(审查 R5-C1): 只挂
// capability-protected 的 Worker 端点(如 /v1/worker/sophub/*), 不注册任何
// 管理/用户路由; 由部署显式传 --worker-internal-listen 启用, 默认关闭。
// 与主 API 共享相同的 timeout/recover/body-limit 中间件, 但不要求 loopback——
// 独立 Runner 容器经 runner-control 网络访问。
func ServeInternalContext(ctx context.Context, addr string, h http.Handler, maxBodyBytes int64) error {
	if addr == "" {
		return fmt.Errorf("internal listener address is required")
	}
	return serveUntil(ctx, addr, h, maxBodyBytes)
}

func serveUntil(ctx context.Context, addr string, h http.Handler, maxBodyBytes int64) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serveListener(ctx, ln, h, maxBodyBytes)
}

// ServeUnixContext 在 unix socket 上运行主 API(round10 审查 B1c): 独立
// nginx 容器经共享卷挂载该 socket 代理 /v1/, 使 Web/API 面不暴露给
// runner-control 网络(Platform 必须接入 runner-control 拨号 Runner, 因此
// 主 API 不能监听 0.0.0.0)。socket 权限 0660、属组 10001(Platform 主组),
// web 容器 group_add 10001 后即可读写。与 loopback TCP listener 共用
// timeout/recover/body-limit 中间件; 退出时删除 socket 文件。
func ServeUnixContext(ctx context.Context, path string, h http.Handler, maxBodyBytes int64) error {
	if path == "" {
		return fmt.Errorf("unix listen path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o2750); err != nil {
		return fmt.Errorf("create unix listen dir: %w", err)
	}
	_ = os.Remove(path) // 清理上次进程残留(同进程重复调用/崩溃遗留)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod unix socket: %w", err)
	}
	defer os.Remove(path)
	return serveListener(ctx, ln, h, maxBodyBytes)
}

func serveListener(ctx context.Context, ln net.Listener, h http.Handler, maxBodyBytes int64) error {
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultMaxRequestBodyBytes
	}
	wrapped := recoverMiddleware(bodyLimitMiddleware(maxBodyBytes)(h))
	srv := &http.Server{
		Handler:           wrapped,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shctx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// bodyLimitMiddleware wraps r.Body with http.MaxBytesReader so any single
// request body beyond max bytes is rejected with 413.
func bodyLimitMiddleware(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}

// recoverMiddleware catches panics from handlers, logs the stack trace, and
// returns a generic 500 so internal details (SQL errors, file paths, stack
// frames) don't leak to clients.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.ErrorContext(r.Context(), "api: panic recovered",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
				)
				writeErr(w, http.StatusInternalServerError, "INTERNAL", "internal server error", traceID())
			}
		}()
		next.ServeHTTP(w, r)
	})
}
