// Package uiaddress validates the one explicit trusted browser-facing authority.
package uiaddress

import (
	"net"
	"net/url"
	"operator-dynamo/internal/contracts"
	"strconv"
	"strings"
)

func Origin(value string) (string, error) {
	if strings.Contains(value, "#") {
		return "", contracts.ErrInvalid
	}
	u, e := url.Parse(value)
	if e != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || strings.Contains(u.Host, "%") {
		return "", contracts.ErrInvalid
	}
	host, port, e := net.SplitHostPort(u.Host)
	if e != nil {
		return "", contracts.ErrInvalid
	}
	n, e := strconv.Atoi(port)
	ip := net.ParseIP(host)
	if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port || (host != "localhost" && (ip == nil || ip.IsUnspecified())) {
		return "", contracts.ErrInvalid
	}
	return "http://" + u.Host, nil
}

// Authority canonicalizes only the admitted IP/localhost HTTP authority profile.
// Browser serialization may omit :80 and compress IPv6 addresses.
func Authority(value string) (string, error) {
	if strings.ContainsAny(value, "/?#@%\\ \t\r\n") {
		return "", contracts.ErrInvalid
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		host = value
		port = "80"
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		} else if strings.Contains(host, ":") {
			return "", contracts.ErrInvalid
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", contracts.ErrInvalid
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || ip.IsUnspecified() {
			return "", contracts.ErrInvalid
		}
		host = ip.String()
	}
	return net.JoinHostPort(host, port), nil
}
func BrowserOrigin(value string) (string, error) {
	if !strings.HasPrefix(value, "http://") {
		return "", contracts.ErrInvalid
	}
	return Authority(strings.TrimPrefix(value, "http://"))
}
