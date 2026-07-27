package llmproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

type ipResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// NetworkPolicy is immutable after construction. Configured CIDRs form a
// strict allowlist; without them, only globally routable public addresses pass.
type NetworkPolicy struct {
	allowedCIDRs     []*net.IPNet
	allowedHTTPHosts map[string]struct{}
	resolver         ipResolver
}

// NewNetworkPolicy parses deployment-owned egress allowlists.
func NewNetworkPolicy(allowedCIDRs, allowedHTTPHosts []string) (*NetworkPolicy, error) {
	policy := &NetworkPolicy{
		allowedCIDRs:     make([]*net.IPNet, 0, len(allowedCIDRs)),
		allowedHTTPHosts: make(map[string]struct{}, len(allowedHTTPHosts)),
		resolver:         net.DefaultResolver,
	}
	for _, raw := range allowedCIDRs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("invalid allowed upstream CIDR %q: %w", raw, err)
		}
		policy.allowedCIDRs = append(policy.allowedCIDRs, network)
	}
	for _, raw := range allowedHTTPHosts {
		host, err := normalizeAllowedHost(raw)
		if err != nil {
			return nil, err
		}
		policy.allowedHTTPHosts[host] = struct{}{}
	}
	return policy, nil
}

func (p *NetworkPolicy) ValidateURL(ctx context.Context, target *url.URL) error {
	if p == nil || p.resolver == nil {
		return errors.New("network policy is not configured")
	}
	if target == nil || target.Host == "" || target.User != nil || target.Fragment != "" || target.Opaque != "" {
		return errors.New("target must be an absolute hierarchical URL without credentials or fragment")
	}
	scheme := strings.ToLower(target.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("unsupported target scheme %q", target.Scheme)
	}
	if scheme == "http" && !p.httpHostAllowed(target) {
		return fmt.Errorf("plain HTTP target host %q is not allowed", target.Host)
	}
	_, err := p.resolveAllowed(ctx, target.Hostname())
	return err
}

func (p *NetworkPolicy) resolveAllowed(ctx context.Context, host string) ([]net.IP, error) {
	var addresses []net.IP
	if literal := net.ParseIP(host); literal != nil {
		addresses = []net.IP{literal}
	} else {
		resolved, err := p.resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve upstream host %q: %w", host, err)
		}
		addresses = make([]net.IP, 0, len(resolved))
		for _, address := range resolved {
			addresses = append(addresses, address.IP)
		}
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("upstream host %q resolved to no addresses", host)
	}
	for _, address := range addresses {
		if !p.ipAllowed(address) {
			return nil, fmt.Errorf("upstream host %q resolved outside network policy", host)
		}
	}
	return addresses, nil
}

func (p *NetworkPolicy) ipAllowed(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if _, metadata := metadataServiceAddresses[address]; metadata {
		return false
	}
	if address.IsUnspecified() || address.IsMulticast() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return false
	}
	if len(p.allowedCIDRs) > 0 {
		for _, network := range p.allowedCIDRs {
			if network.Contains(ip) {
				return true
			}
		}
		return false
	}
	return isPublicUnicast(address)
}

func (p *NetworkPolicy) httpHostAllowed(target *url.URL) bool {
	host := strings.ToLower(target.Hostname())
	hostPort := strings.ToLower(target.Host)
	_, hostAllowed := p.allowedHTTPHosts[host]
	_, hostPortAllowed := p.allowedHTTPHosts[hostPort]
	return hostAllowed || hostPortAllowed
}

func normalizeAllowedHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" || strings.ContainsAny(host, "/?#@ \t\r\n") || strings.Contains(host, "://") {
		return "", fmt.Errorf("invalid allowed HTTP host %q", raw)
	}
	if strings.HasPrefix(host, "[") {
		if parsed := net.ParseIP(strings.Trim(host, "[]")); parsed == nil {
			_, port, err := net.SplitHostPort(host)
			if err != nil || !validPort(port) {
				return "", fmt.Errorf("invalid allowed HTTP host %q", raw)
			}
		}
	} else if strings.Count(host, ":") == 1 {
		_, port, err := net.SplitHostPort(host)
		if err != nil || !validPort(port) {
			return "", fmt.Errorf("invalid allowed HTTP host %q", raw)
		}
	} else if strings.Count(host, ":") > 1 && net.ParseIP(host) == nil {
		return "", fmt.Errorf("invalid allowed HTTP host %q", raw)
	}
	return host, nil
}

func validPort(raw string) bool {
	port, err := strconv.Atoi(raw)
	return err == nil && port > 0 && port <= 65535
}

var metadataServiceAddresses = map[netip.Addr]struct{}{
	netip.MustParseAddr("169.254.169.254"): {},
	netip.MustParseAddr("100.100.100.200"): {},
	netip.MustParseAddr("fd00:ec2::254"):   {},
}

var blockedPublicPrefixes = []netip.Prefix{

	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func isPublicUnicast(address netip.Addr) bool {
	if !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range blockedPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
