package roster

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

var blockedNetworks = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3",
		"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Zone() != "" {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, p := range blockedNetworks {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func publicURL(raw string) (*url.URL, error) {
	invalid := errors.New("Roster URL must be public HTTPS without credentials or query parameters.")
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Hostname() == "" || strings.ContainsAny(raw, "\\#") {
		return nil, invalid
	}
	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil {
		if !publicIP(ip) {
			return nil, invalid
		}
	} else {
		if len(host) > 253 || !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
			return nil, invalid
		}
		for _, suffix := range []string{".localhost", ".local", ".internal", ".home", ".lan"} {
			if strings.HasSuffix(host, suffix) {
				return nil, invalid
			}
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, invalid
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return nil, invalid
				}
			}
		}
	}
	return u, nil
}

func publicClient(resolver Resolver) *http.Client {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil, // Ambient proxy configuration must not bypass DNS/IP validation.
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          2,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, errors.New("Invalid roster host.")
			}
			ips, err := resolver.LookupIPAddr(ctx, host)
			if err != nil || len(ips) == 0 {
				return nil, errors.New("Roster DNS lookup failed.")
			}
			for _, ip := range ips {
				a, ok := netip.AddrFromSlice(ip.IP)
				if !ok || ip.Zone != "" || !publicIP(a) {
					return nil, errors.New("Roster host is not public.")
				}
			}
			// Dial the validated address, never the hostname (no second DNS lookup).
			// Transport still checks TLS against the original URL hostname.
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if err == nil {
					return conn, nil
				}
			}
			return nil, errors.New("Roster connection failed.")
		},
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: noRedirect}
}

func noRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
