package llmproxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type trackingRequestBody struct {
	io.Reader
	closed bool
}

func (b *trackingRequestBody) Close() error {
	b.closed = true
	return nil
}

type fakeIPResolver struct {
	addresses map[string][]net.IPAddr
	err       error
}

func (r *fakeIPResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if r.err != nil {
		return nil, r.err
	}
	return append([]net.IPAddr(nil), r.addresses[host]...), nil
}

type sequenceIPResolver struct {
	responses [][]net.IPAddr
	calls     int
}

func (r *sequenceIPResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	index := r.calls
	r.calls++
	if index >= len(r.responses) {
		index = len(r.responses) - 1
	}
	return append([]net.IPAddr(nil), r.responses[index]...), nil
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestNetworkPolicyRejectsUnsafeTargetsAndHTTPByDefault(t *testing.T) {
	policy, err := NewNetworkPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy.resolver = &fakeIPResolver{addresses: map[string][]net.IPAddr{
		"public.example": {{IP: net.ParseIP("93.184.216.34")}},
	}}
	if err := policy.ValidateURL(context.Background(), mustURL(t, "https://public.example/v1")); err != nil {
		t.Fatalf("public HTTPS rejected: %v", err)
	}
	for _, raw := range []string{
		"http://public.example/v1",
		"https://127.0.0.1/v1",
		"https://169.254.169.254/latest/meta-data",
		"https://[::1]/v1",
	} {
		if err := policy.ValidateURL(context.Background(), mustURL(t, raw)); err == nil {
			t.Fatalf("unsafe target accepted: %s", raw)
		}
	}
}

func TestNetworkPolicyEnforcesConfiguredCIDRsAndHTTPHosts(t *testing.T) {
	policy, err := NewNetworkPolicy([]string{"10.20.0.0/16"}, []string{"internal.example"})
	if err != nil {
		t.Fatal(err)
	}
	policy.resolver = &fakeIPResolver{addresses: map[string][]net.IPAddr{
		"internal.example": {{IP: net.ParseIP("10.20.1.5")}},
		"outside.example":  {{IP: net.ParseIP("10.21.1.5")}},
	}}
	if err := policy.ValidateURL(context.Background(), mustURL(t, "http://internal.example/v1")); err != nil {
		t.Fatalf("explicit internal target rejected: %v", err)
	}
	for _, raw := range []string{
		"http://outside.example/v1",
		"http://10.20.1.5/v1",
		"https://outside.example/v1",
	} {
		if err := policy.ValidateURL(context.Background(), mustURL(t, raw)); err == nil {
			t.Fatalf("target outside policy accepted: %s", raw)
		}
	}
}

func TestTransportUsesExplicitLoopbackPolicyAndDoesNotFollowRedirects(t *testing.T) {
	targetHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			w.Header().Set("Location", "/target")
			w.WriteHeader(http.StatusFound)
		case "/target":
			targetHits++
			_, _ = w.Write([]byte("target"))
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer server.Close()

	policy, err := NewNetworkPolicy([]string{"127.0.0.0/8"}, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewTransportCache(policy)
	if err != nil {
		t.Fatal(err)
	}
	provider := testProvider(domain.ProviderNativeOAI, server.URL, "model", "")
	provider.ID = 1
	roundTripper, err := cache.RoundTripper(provider)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/redirect", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := roundTripper.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusFound || targetHits != 0 {
		t.Fatalf("status=%d target_hits=%d", response.StatusCode, targetHits)
	}
}

func TestTransportCacheReusesIdenticalConfigAndChangesOnConfigUpdate(t *testing.T) {
	policy, err := NewNetworkPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy.resolver = &fakeIPResolver{addresses: map[string][]net.IPAddr{
		"api.example": {{IP: net.ParseIP("93.184.216.34")}},
	}}
	cache, err := NewTransportCache(policy)
	if err != nil {
		t.Fatal(err)
	}
	provider := testProvider(domain.ProviderNativeOAI, "https://api.example", "model", "")
	provider.ID = 1
	first, err := cache.RoundTripper(provider)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.RoundTripper(provider)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("identical transport config did not reuse cached RoundTripper")
	}

	timeout := int((15 * time.Second).Seconds())
	provider.TransportConfig.ConnectTimeoutSeconds = &timeout
	changed, err := cache.RoundTripper(provider)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("transport config change reused stale RoundTripper")
	}
	otherProvider := provider
	otherProvider.ID = 2
	reused, err := cache.RoundTripper(otherProvider)
	if err != nil {
		t.Fatal(err)
	}
	if reused == changed {
		t.Fatal("different Providers shared a connection pool")
	}
}

func TestTransportCacheConcurrentCallsShareProviderPool(t *testing.T) {
	policy, err := NewNetworkPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy.resolver = &fakeIPResolver{addresses: map[string][]net.IPAddr{
		"api.example": {{IP: net.ParseIP("93.184.216.34")}},
	}}
	cache, err := NewTransportCache(policy)
	if err != nil {
		t.Fatal(err)
	}
	provider := testProvider(domain.ProviderNativeOAI, "https://api.example", "model", "")
	provider.ID = 1
	const callers = 16
	results := make(chan http.RoundTripper, callers)
	errorsSeen := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			roundTripper, err := cache.RoundTripper(provider)
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- roundTripper
		}()
	}
	wait.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
	var first http.RoundTripper
	for result := range results {
		if first == nil {
			first = result
			continue
		}
		if result != first {
			t.Fatal("concurrent cache calls created multiple connection pools")
		}
	}
}

func TestPolicyRoundTripperClosesRejectedRequestBody(t *testing.T) {
	policy, err := NewNetworkPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := &trackingRequestBody{Reader: http.NoBody}
	request, err := http.NewRequest(http.MethodPost, "https://127.0.0.1/v1", body)
	if err != nil {
		t.Fatal(err)
	}
	roundTripper := &policyRoundTripper{policy: policy, transport: http.DefaultTransport.(*http.Transport).Clone()}
	if response, err := roundTripper.RoundTrip(request); err == nil || response != nil {
		t.Fatalf("unsafe request accepted: response=%v err=%v", response, err)
	}
	if !body.closed {
		t.Fatal("rejected request body was not closed")
	}
}

func TestTransportRevalidatesDNSImmediatelyBeforeDial(t *testing.T) {
	policy, err := NewNetworkPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &sequenceIPResolver{responses: [][]net.IPAddr{
		{{IP: net.ParseIP("93.184.216.34")}},
		{{IP: net.ParseIP("93.184.216.34")}},
		{{IP: net.ParseIP("127.0.0.1")}},
	}}
	policy.resolver = resolver
	cache, err := NewTransportCache(policy)
	if err != nil {
		t.Fatal(err)
	}
	provider := testProvider(domain.ProviderNativeOAI, "https://rebind.example", "model", "")
	provider.ID = 1
	roundTripper, err := cache.RoundTripper(provider)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://rebind.example/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := roundTripper.RoundTrip(request); err == nil || response != nil {
		t.Fatalf("DNS rebinding was not rejected: response=%v err=%v", response, err)
	}
	if resolver.calls < 3 {
		t.Fatalf("DNS lookups=%d want at least 3", resolver.calls)
	}
}

func TestNetworkPolicyNeverAllowsMetadataOrLinkLocal(t *testing.T) {
	policy, err := NewNetworkPolicy(
		[]string{"0.0.0.0/0"},
		[]string{"169.254.169.254", "169.254.1.1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data",
		"http://169.254.1.1/internal",
	} {
		if err := policy.ValidateURL(context.Background(), mustURL(t, raw)); err == nil {
			t.Fatalf("permanently unsafe target accepted: %s", raw)
		}
	}
}

func TestNetworkPolicyRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewNetworkPolicy([]string{"not-a-cidr"}, nil); err == nil {
		t.Fatal("expected invalid CIDR error")
	}

	if _, err := NewNetworkPolicy(nil, []string{"https://not-a-host/path"}); err == nil {
		t.Fatal("expected invalid HTTP host error")
	}
	if _, err := NewNetworkPolicy(nil, []string{"internal.example:not-a-port"}); err == nil {
		t.Fatal("expected invalid HTTP host port error")
	}
	config := testConfig()
	config.AllowedUpstreamCIDRs = []string{"invalid"}
	if err := config.Validate(); err == nil {
		t.Fatal("Config accepted invalid network policy")
	}
}
