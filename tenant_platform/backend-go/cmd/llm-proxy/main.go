// Command llm-proxy is the LLM Proxy deployment unit: the sole holder of the
// real upstream LLM key. It validates short-lived session capability_tokens
// and forwards approved requests upstream. Binds loopback only.
//
// The proxy requires database provider mode so capability revocation remains
// effective across process restarts. Provider secrets are read only through
// the encrypted platform store.
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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/secret"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/infrastructure/systemd"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "llm-proxy: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen       = flag.String("listen", firstNonEmpty(os.Getenv("LLM_PROXY_LISTEN"), "127.0.0.1:8081"), "listen address (or LLM_PROXY_LISTEN)")
		signingKey   = flag.String("capability-signing-key", "", "HMAC signing key for capability_tokens (or LLM_PROXY_CAPABILITY_SIGNING_KEY)")
		tokenTTL     = flag.Duration("token-ttl", llmproxy.DefaultTokenTTL, "capability_token lifetime")
		databaseURL  = flag.String("database-url", "", "PostgreSQL URL to read admin-configured providers (or DATABASE_URL)")
		cipherKeyHex = flag.String("llm-provider-key", firstNonEmpty(os.Getenv("LLM_PROVIDER_KEY"), os.Getenv("BOT_TOKEN_KEY")), "AES-256-GCM hex key for decrypting provider API keys (or LLM_PROVIDER_KEY; falls back to BOT_TOKEN_KEY — must equal the key Platform uses to encrypt provider keys)")
	)
	flag.Parse()

	key := []byte(firstNonEmpty(*signingKey, os.Getenv("LLM_PROXY_CAPABILITY_SIGNING_KEY")))
	if len(key) < llmproxy.MinSigningKeyLen {
		return fmt.Errorf("capability signing key must be at least %d bytes", llmproxy.MinSigningKeyLen)
	}

	cipher, err := loadCipher(*cipherKeyHex, key)
	if err != nil {
		return fmt.Errorf("load cipher: %w", err)
	}

	providerSource, revocations, usageCounter, taskChecker, providerCleanup, err := buildProviderSource(firstNonEmpty(*databaseURL, os.Getenv("DATABASE_URL")))
	if err != nil {
		return err
	}
	if providerCleanup != nil {
		defer providerCleanup()
	}

	cfg := llmproxy.Config{
		Listen:               *listen,
		SigningKey:           key,
		TokenTTL:             *tokenTTL,
		ProviderSource:       providerSource,
		Cipher:               cipher,
		Revocations:          revocations,
		// round9 审查: 在线 task/lease/成员校验(成员移除/接管即时生效)。
		TaskChecker:          taskChecker,
		// 审查 R4-I9: llm-proxy 有 DB 访问, 注入 store 作为按 JTI 的
		// 预算计量后端, 转发前原子消费 max_turns。
		UsageCounter:         usageCounter,
		AllowedUpstreamCIDRs: llmproxy.ParseNetworkPolicyList(os.Getenv("LLM_PROXY_ALLOWED_UPSTREAM_CIDRS")),
		AllowedHTTPHosts:     llmproxy.ParseNetworkPolicyList(os.Getenv("LLM_PROXY_ALLOW_HTTP_HOSTS")),
	}

	srv, err := llmproxy.NewServer(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	httpSrv := llmproxy.NewHTTPServer(cfg.Listen, srv.Handler())

	serve := func() error {
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
			// Derive shutdown ctx from Background, not ctx (which is already cancelled).
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer shutCancel()
			return httpSrv.Shutdown(shutCtx)
		case err := <-errCh:
			return err
		}
	}

	return systemd.ReadyAndServe(ctx, serve)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func loadCipher(cipherKeyHex string, signingKey []byte) (secret.TokenCipher, error) {
	_ = signingKey
	if cipherKeyHex == "" {
		return nil, fmt.Errorf("--llm-provider-key (or LLM_PROVIDER_KEY, falling back to BOT_TOKEN_KEY) is required; the AES-256-GCM key must be provided explicitly from a secret manager and must equal the key Platform uses to encrypt provider API keys")
	}
	return secret.NewStaticKeyCipherFromHex(cipherKeyHex)
}

func buildProviderSource(databaseURL string) (llmproxy.ProviderSource, llmproxy.CapabilityRevocationSource, llmproxy.CapabilityUsageCounter, llmproxy.TaskCapabilityChecker, func(), error) {
	if databaseURL == "" {
		return nil, nil, nil, nil, nil, fmt.Errorf("--database-url (or DATABASE_URL) is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("connect postgres: %w", err)
	}
	store, err := postgres.NewStore(pool)
	if err != nil {
		pool.Close()
		return nil, nil, nil, nil, nil, fmt.Errorf("create store: %w", err)
	}
	// round9 审查: store 同时承担在线 task 活跃性校验(IsTaskCapabilityActive)。
	return store, store, store, store, pool.Close, nil
}
