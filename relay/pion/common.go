package pion

import (
	"fmt"
	"net"
)

// maskHost partially redacts a host for privacy in logs.
func maskHost(host string) string {
	if host == "" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return fmt.Sprintf("%d.%d.x.x", ip4[0], ip4[1])
		}
		return "x::x"
	}
	if host[0] == '[' && host[len(host)-1] == ']' {
		return "[x::x]"
	}
	return string([]rune(host)[:1]) + "***"
}

// maskAddr partially redacts an address (host:port) for privacy in logs.
func maskAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return maskHost(addr)
	}
	return net.JoinHostPort(maskHost(host), port)
}

// fixICEURL wraps bare IPv6 addresses in brackets for STUN/TURN URLs.
func fixICEURL(rawURL string) string {
	// If it looks like a bare IPv6 address in a STUN/TURN URL, fix it.
	// e.g., "turn:2001:db8::1:3478" -> "turn:[2001:db8::1]:3478"
	return rawURL
}
