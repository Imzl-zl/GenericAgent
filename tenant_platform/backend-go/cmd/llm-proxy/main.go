// Command llm-proxy is the LLM Proxy deployment unit: the sole holder of the
// real upstream LLM key. It validates short-lived session capability_tokens
// and forwards approved requests upstream. Binds loopback only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/llmproxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "llm-proxy: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen          = flag.String("listen", "127.0.0.1:8081", "loopback listen address")
		upstreamBaseURL = flag.String("upstream-base-url", "", "OpenAI-compatible upstream base URL (or LLM_PROXY_UPSTREAM_BASEURL)")
		upstreamAPIKey  = flag.String("upstream-apikey", "", "real upstream API key (or LLM_PROXY_UPSTREAM_APIKEY); prefer host env")
		signingKey      = flag.String("capability-signing-key", "", "HMAC signing key for capability_tokens (or LLM_PROXY_CAPABILITY_SIGNING_KEY)")
		tokenTTL        = flag.Duration("token-ttl", llmproxy.DefaultTokenTTL, "capability_token lifetime")
	)
	flag.Parse()

	cfg := llmproxy.Config{
		Listen:          *listen,
		UpstreamBaseURL: firstNonEmpty(*upstreamBaseURL, os.Getenv("LLM_PROXY_UPSTREAM_BASEURL")),
		UpstreamAPIKey:  firstNonEmpty(*upstreamAPIKey, os.Getenv("LLM_PROXY_UPSTREAM_APIKEY")),
		SigningKey:      []byte(firstNonEmpty(*signingKey, os.Getenv("LLM_PROXY_CAPABILITY_SIGNING_KEY"))),
		TokenTTL:        *tokenTTL,
	}

	srv, err := llmproxy.NewServer(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "llm-proxy: listen=%s token_ttl=%s\n", cfg.Listen, cfg.TokenTTL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		return httpSrv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
