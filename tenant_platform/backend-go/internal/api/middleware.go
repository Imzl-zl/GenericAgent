package api

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// ctxKey is the unexported context key type for HTTP middleware values.
type ctxKey int

const ctxUserIDKey ctxKey = 0

// auth wraps a handler with the platform dev-token check. The dev token is a
// single shared secret used by smoke tooling and the loopback admin path; it
// is NOT a user authentication mechanism. User-authenticated endpoints use
// userAuth instead.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Platform-Dev-Token")
		if tok == "" || tok != s.devToken {
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid X-Platform-Dev-Token", traceID())
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
// no user is authenticated (dev-token path).
func userIDFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(ctxUserIDKey).(int64)
	return v, ok
}

// ServeContext runs the HTTP server with sane timeouts and middleware until
// ctx is cancelled. The handler chain applies: body-size limit (prevents
// memory exhaustion), panic recovery (logs stack trace, returns 500 without
// leaking internals). Timeouts protect against Slowloris and stuck writers.
func ServeContext(ctx context.Context, addr string, h http.Handler) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("platform API must bind loopback, got %s", addr)
	}
	wrapped := recoverMiddleware(bodyLimitMiddleware(MaxRequestBodyBytes)(h))
	srv := &http.Server{
		Addr:              addr,
		Handler:           wrapped,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
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
