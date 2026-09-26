// Package allowlist restricts access by client IP.
package allowlist

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// List is a set of allowed IPs/CIDRs. A nil or empty List allows everything.
type List struct {
	nets []*net.IPNet
}

// Parse reads a comma/space separated list of IPs and CIDRs.
func Parse(s string) (*List, error) {
	l := &List{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if !strings.Contains(f, "/") {
			ip := net.ParseIP(f)
			if ip == nil {
				return nil, fmt.Errorf("invalid IP %q", f)
			}
			if ip4 := ip.To4(); ip4 != nil {
				f += "/32"
			} else {
				f += "/128"
			}
		}
		_, n, err := net.ParseCIDR(f)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", f, err)
		}
		l.nets = append(l.nets, n)
	}
	return l, nil
}

var (
	localRanges   = "127.0.0.0/8,::1/128"
	networkRanges = localRanges + ",10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,100.64.0.0/10,fc00::/7,fe80::/10"
)

// New builds a List from an access mode and extra IPs/CIDRs.
//
//	local:    loopback only (+ extra)
//	network:  loopback and private/LAN ranges (+ extra)
//	internet: anyone, unless extra is set, in which case only extra
func New(mode, extra string) (*List, error) {
	var base string
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "local":
		base = localRanges
	case "network":
		base = networkRanges
	case "internet", "":
	default:
		return nil, fmt.Errorf("invalid access mode %q (want local, network or internet)", mode)
	}
	if base == "" && strings.TrimSpace(extra) == "" {
		return &List{}, nil
	}
	if base != "" && strings.TrimSpace(extra) != "" {
		base += ","
	}
	return Parse(base + extra)
}

func (l *List) Enabled() bool { return l != nil && len(l.nets) > 0 }

func (l *List) Allows(ip net.IP) bool {
	if !l.Enabled() {
		return true
	}
	for _, n := range l.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// AllowsAddr checks a "host:port" address (as from RemoteAddr).
func (l *List) AllowsAddr(addr string) bool {
	if !l.Enabled() {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && l.Allows(ip)
}

// Middleware rejects HTTP requests from disallowed client IPs.
func (l *List) Middleware(next http.Handler) http.Handler {
	if !l.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.AllowsAddr(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
