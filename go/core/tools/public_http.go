package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const publicHTTPDNSLookupTimeout = 3 * time.Second

// publicHTTPResolver is deliberately package-private. Tests can provide a
// deterministic resolver without creating a production switch that weakens
// the network boundary.
type publicHTTPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// publicHTTPPolicy centralises the outbound HTTP trust boundary used by tools
// that fetch model-provided URLs. Production uses the system resolver and a
// net.Dialer. Tests may replace either dependency to prove DNS and redirect
// behaviour without contacting the public internet.
type publicHTTPPolicy struct {
	resolver    publicHTTPResolver
	dialContext func(context.Context, string, string) (net.Conn, error)
}

type validatedPublicHTTPTarget struct {
	host string
	ips  []net.IP
}

type validatedPublicHTTPTargetKey struct{}

func (p *publicHTTPPolicy) resolverOrDefault() publicHTTPResolver {
	if p != nil && p.resolver != nil {
		return p.resolver
	}
	return net.DefaultResolver
}

func (p *publicHTTPPolicy) dialerOrDefault() func(context.Context, string, string) (net.Conn, error) {
	if p != nil && p.dialContext != nil {
		return p.dialContext
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return dialer.DialContext
}

// resolvePublicHTTPHost resolves a hostname once, rejects the entire answer
// set if any address is non-public, then returns a copy that can be pinned to
// the request's dial. Rejecting mixed public/private answers prevents a DNS
// response from hiding an internal address behind a public first record.
func (p *publicHTTPPolicy) resolvePublicHTTPHost(ctx context.Context, host string) ([]net.IP, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return nil, fmt.Errorf("host is required")
	}
	if publicHTTPHostnameIsBlocked(host) {
		return nil, fmt.Errorf("private, local, and metadata network targets are unavailable")
	}
	if strings.Contains(host, "%") {
		return nil, fmt.Errorf("scoped network targets are unavailable")
	}
	if literal := net.ParseIP(host); literal != nil {
		if publicHTTPIPIsBlocked(literal) {
			return nil, fmt.Errorf("private, local, and metadata network targets are unavailable")
		}
		return []net.IP{append(net.IP(nil), literal...)}, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, publicHTTPDNSLookupTimeout)
	defer cancel()
	addresses, err := p.resolverOrDefault().LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}

	seen := make(map[string]struct{}, len(addresses))
	ips := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		if publicHTTPIPIsBlocked(address.IP) {
			return nil, fmt.Errorf("host %s resolves to a private, local, or metadata network target", host)
		}
		key := address.IP.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		ips = append(ips, append(net.IP(nil), address.IP...))
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no usable addresses", host)
	}
	return ips, nil
}

func publicHTTPHostnameIsBlocked(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		host == "metadata" || strings.HasSuffix(host, ".metadata") ||
		host == "metadata.google.internal" ||
		host == "instance-data.ec2.internal" ||
		strings.HasSuffix(host, ".local") {
		return true
	}
	return false
}

var publicHTTPBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // shared address space; includes some metadata services
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), // benchmark networks are never public destinations
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func publicHTTPIPIsBlocked(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		!ip.IsGlobalUnicast() {
		return true
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	address = address.Unmap()
	// Azure WireServer exposes host/metadata services at a globally-routable
	// looking address; it must not pass a generic IsPrivate check.
	if address == netip.MustParseAddr("168.63.129.16") {
		return true
	}
	for _, prefix := range publicHTTPBlockedPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func (p *publicHTTPPolicy) validateURL(ctx context.Context, target *url.URL) ([]net.IP, error) {
	if target == nil || (target.Scheme != "http" && target.Scheme != "https") ||
		target.Host == "" || target.Hostname() == "" {
		return nil, fmt.Errorf("invalid http(s) URL")
	}
	return p.resolvePublicHTTPHost(ctx, target.Hostname())
}

func withValidatedPublicHTTPTarget(request *http.Request, ips []net.IP) *http.Request {
	if request == nil || request.URL == nil {
		return request
	}
	validated := validatedPublicHTTPTarget{
		host: strings.TrimSuffix(strings.ToLower(request.URL.Hostname()), "."),
		ips:  cloneIPs(ips),
	}
	return request.WithContext(context.WithValue(request.Context(), validatedPublicHTTPTargetKey{}, validated))
}

func cloneIPs(ips []net.IP) []net.IP {
	copyOfIPs := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		copyOfIPs = append(copyOfIPs, append(net.IP(nil), ip...))
	}
	return copyOfIPs
}

type publicHTTPGuardRoundTripper struct {
	base   http.RoundTripper
	policy *publicHTTPPolicy
}

func (t *publicHTTPGuardRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, fmt.Errorf("invalid http request")
	}
	validated, _ := request.Context().Value(validatedPublicHTTPTargetKey{}).(validatedPublicHTTPTarget)
	host := strings.TrimSuffix(strings.ToLower(request.URL.Hostname()), ".")
	if validated.host != host || len(validated.ips) == 0 {
		ips, err := t.policy.validateURL(request.Context(), request.URL)
		if err != nil {
			return nil, err
		}
		request = withValidatedPublicHTTPTarget(request, ips)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(request)
}

func (p *publicHTTPPolicy) dialValidated(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse outbound address %q: %w", address, err)
	}
	normalizedHost := strings.TrimSuffix(strings.ToLower(host), ".")
	validated, _ := ctx.Value(validatedPublicHTTPTargetKey{}).(validatedPublicHTTPTarget)
	ips := validated.ips
	if validated.host != normalizedHost || len(ips) == 0 {
		ips, err = p.resolvePublicHTTPHost(ctx, normalizedHost)
		if err != nil {
			return nil, err
		}
	}

	dial := p.dialerOrDefault()
	var dialErrors []error
	for _, ip := range ips {
		if network == "tcp4" && ip.To4() == nil {
			continue
		}
		if network == "tcp6" && ip.To4() != nil {
			continue
		}
		conn, dialErr := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		dialErrors = append(dialErrors, dialErr)
	}
	if len(dialErrors) == 0 {
		return nil, fmt.Errorf("connect %s: no usable addresses", normalizedHost)
	}
	return nil, fmt.Errorf("connect %s: %w", normalizedHost, errors.Join(dialErrors...))
}

// guardPublicHTTPClient returns a shallow client copy with three guarantees:
// every request and redirect is resolved fail-closed, resolved IPs are pinned
// to the dial to close DNS-rebinding races, and environment proxies cannot
// perform a second unvalidated resolution. Non-Transport RoundTrippers are a
// trusted test/integration seam; URLs are still validated before they run.
func guardPublicHTTPClient(base *http.Client, allowPrivateTestTargets bool, policy *publicHTTPPolicy) *http.Client {
	if base == nil {
		base = &http.Client{}
	}
	guarded := *base
	if allowPrivateTestTargets {
		return &guarded
	}
	if policy == nil {
		policy = &publicHTTPPolicy{}
	}

	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if standard, ok := transport.(*http.Transport); ok {
		cloned := standard.Clone()
		// Proxy servers resolve the destination independently, which would reopen
		// the rebinding/SSRF gap after local validation. Tool fetches therefore
		// use a direct, IP-pinned connection.
		cloned.Proxy = nil
		cloned.DialContext = policy.dialValidated
		cloned.DialTLSContext = nil
		transport = cloned
	}
	guarded.Transport = &publicHTTPGuardRoundTripper{base: transport, policy: policy}

	originalRedirect := base.CheckRedirect
	guarded.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		ips, err := policy.validateURL(request.Context(), request.URL)
		if err != nil {
			return err
		}
		*request = *withValidatedPublicHTTPTarget(request, ips)
		if originalRedirect != nil {
			return originalRedirect(request, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return &guarded
}
