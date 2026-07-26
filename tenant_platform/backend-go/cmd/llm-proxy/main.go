// Command llm-proxy is the LLM Proxy deployment unit: the sole holder of the
// real upstream LLM key. It validates short-lived session capability_tokens
// and forwards approved requests upstream. Binds loopback only.
//
// The proxy can run in two modes:
//   - Static provider mode (standalone/dev): configure --upstream-base-url,
//     --upstream-apikey, --provider-type and --model directly.
//   - Database provider mode: pass --database-url and --llm-provider-key; the
//     proxy reads the admin-configured default provider from the platform DB.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/llmproxy"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/postgres"
	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/secret"
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
		providerType    = flag.String("provider-type", string(domain.ProviderOpenAICompatible), "provider type: openai_compatible | anthropic_messages")
		model           = flag.String("model", "gpt-4o-mini", "model identifier forwarded upstream")
		databaseURL     = flag.String("database-url", "", "PostgreSQL URL to read admin-configured providers (or DATABASE_URL)")
		cipherKeyHex    = flag.String("llm-provider-key", os.Getenv("LLM_PROVIDER_KEY"), "AES-256-GCM hex key for decrypting provider API keys (or LLM_PROVIDER_KEY)")
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

	providerSource, providerCleanup, err := buildProviderSource(*databaseURL, *upstreamBaseURL, *upstreamAPIKey,
		domain.LLMProviderType(*providerType), *model, cipher)
	if err != nil {
		return err
	}
	if providerCleanup != nil {
		defer providerCleanup()
	}

	cfg := llmproxy.Config{
		Listen:         *listen,
		SigningKey:     key,
		TokenTTL:       *tokenTTL,
		ProviderSource: providerSource,
		Cipher:         cipher,
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

func loadCipher(cipherKeyHex string, signingKey []byte) (secret.TokenCipher, error) {
	_ = signingKey
	if cipherKeyHex == "" {
		return nil, fmt.Errorf("--llm-provider-key (or LLM_PROVIDER_KEY) is required; the AES-256-GCM key must be provided explicitly from a secret manager")
	}
	return secret.NewStaticKeyCipherFromHex(cipherKeyHex)
}

func buildProviderSource(databaseURL, upstreamBaseURL, upstreamAPIKey string,
	providerType domain.LLMProviderType, model string, cipher secret.TokenCipher) (llmproxy.ProviderSource, func(), error) {
	if databaseURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			return nil, nil, fmt.Errorf("connect postgres: %w", err)
		}
		store, err := postgres.NewStore(pool)
		if err != nil {
			pool.Close()
			return nil, nil, fmt.Errorf("create store: %w", err)
		}
		// Caller owns pool lifetime; closing it here would break the returned store.
		return store, pool.Close, nil
	}

	if upstreamBaseURL == "" {
		return nil, nil, fmt.Errorf("--upstream-base-url required in static mode (or use --database-url)")
	}
	if upstreamAPIKey == "" {
		return nil, nil, fmt.Errorf("--upstream-apikey required in static mode (or use --database-url)")
	}
	ciphertext, version, err := cipher.Encrypt([]byte(upstreamAPIKey))
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt upstream key: %w", err)
	}
	return &staticProviderSource{
		provider: domain.LLMProvider{
			ProviderType:     providerType,
			BaseURL:          upstreamBaseURL,
			Model:            model,
			APIKeyCiphertext: ciphertext,
			APIKeyKeyVersion: strconv.Itoa(version),
			IsDefault:        true,
			State:            "active",
		},
	}, nil, nil
}

type staticProviderSource struct {
	provider domain.LLMProvider
}

func (s *staticProviderSource) GetDefaultProvider(ctx context.Context) (domain.LLMProvider, error) {
	_ = ctx
	return s.provider, nil
}
