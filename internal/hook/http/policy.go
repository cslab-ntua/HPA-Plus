package http

import (
	"context"
	"errors"
	"net"
	stdhttp "net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ErrDestinationDenied = errors.New("hook destination is not allowed")
var ErrInvalidURL = errors.New("hook URL must use HTTP(S), a valid host/port and no credentials or fragment")

// New constructs the operator-owned hook boundary. An empty allowlist denies
// every request. Origins are exact scheme/host/effective-port tuples, not suffixes.
func New(allowedOrigins []string) (*Execute, error) {
	if len(allowedOrigins) > 64 {
		return nil, errors.New("hook origin allowlist exceeds 64 entries")
	}
	origins := make(map[string]struct{}, len(allowedOrigins))
	for _, raw := range allowedOrigins {
		if len(raw) > 300 {
			return nil, ErrInvalidURL
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, ErrInvalidURL
		}
		key, err := origin(u)
		if err != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery {
			return nil, ErrInvalidURL
		}
		origins[key] = struct{}{}
	}
	// Construct a dedicated transport: environment proxies and custom global
	// DefaultTransport settings must not bypass destination-address checks.
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &stdhttp.Transport{
		DialContext:       guardedDial(net.DefaultResolver.LookupNetIP, dialer.DialContext),
		ForceAttemptHTTP2: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 2,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second, MaxResponseHeaderBytes: 32 * 1024,
	}
	return &Execute{Client: stdhttp.Client{Transport: transport}, origins: origins}, nil
}

func origin(u *url.URL) (string, error) {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.Fragment != "" || strings.Contains(u.Host, "%") {
		return "", ErrInvalidURL
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.HasSuffix(host, ".") {
		return "", ErrInvalidURL
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	} else {
		if len(host) > 253 {
			return "", ErrInvalidURL
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", ErrInvalidURL
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return "", ErrInvalidURL
				}
			}
		}
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return "", ErrInvalidURL
		}
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", ErrInvalidURL
	}
	return u.Scheme + "://" + net.JoinHostPort(host, strconv.Itoa(n)), nil
}

func (e *Execute) authorize(u *url.URL) error {
	key, err := origin(u)
	if err != nil {
		return err
	}
	if _, ok := e.origins[key]; !ok {
		return ErrDestinationDenied
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !allowedAddress(ip) {
		return ErrDestinationDenied
	}
	return nil
}

// Deny non-unicast/local-link destinations and known metadata/platform IPs.
// RFC1918 and ULA addresses remain usable for explicitly allowlisted services.
func allowedAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range deniedPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

var deniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.100.100.200/32"),
	netip.MustParsePrefix("168.63.129.16/32"), netip.MustParsePrefix("fd00:ec2::254/128"),
	netip.MustParsePrefix("fd20:ce::254/128"),
	// Block well-known IPv4 translation/transition ranges rather than allowing
	// an encoded metadata address to bypass the IPv4 checks.
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("2001::/32"),
}

type lookupIP func(context.Context, string, string) ([]netip.Addr, error)
type dialIP func(context.Context, string, string) (net.Conn, error)

func guardedDial(lookup lookupIP, dial dialIP) dialIP {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrDestinationDenied
		}
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else {
			ips, err = lookup(ctx, "ip", host)
			if err != nil {
				return nil, safeRequestError(ctx, err)
			}
		}
		if len(ips) == 0 {
			return nil, ErrDestinationDenied
		}
		// Reject mixed safe/unsafe answers before making any connection.
		for _, ip := range ips {
			if !allowedAddress(ip) {
				return nil, ErrDestinationDenied
			}
		}
		for _, ip := range ips {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			conn, err := dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, safeRequestError(ctx, nil)
	}
}

func safeRequestError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, ErrDestinationDenied) {
		return ErrDestinationDenied
	}
	return errors.New("hook request failed")
}
