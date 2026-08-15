package jetkvm

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// CanonicalBaseURL validates and canonicalizes the direct-device URL shape.
// Userinfo is rejected so credentials can never be smuggled through a CLI
// argument; queries/fragments/paths are rejected because JetKVM's API lives at
// the origin root and accepting aliases would undermine session coordination.
// Errors deliberately never echo raw input.
func CanonicalBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Opaque != "" {
		return "", fmt.Errorf("jetkvm: device URL is invalid")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("jetkvm: device URL scheme must be http or https")
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("jetkvm: device URL must include a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("jetkvm: device URL must not contain userinfo, a query, or a fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("jetkvm: device URL must not contain a path")
	}

	host, err := canonicalHostname(u.Hostname())
	if err != nil {
		return "", err
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("jetkvm: device URL port is invalid")
		}
		port = strconv.Itoa(n)
	}
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return (&url.URL{Scheme: u.Scheme, Host: host}).String(), nil
}

func canonicalHostname(raw string) (string, error) {
	address := raw
	zone := ""
	if i := strings.LastIndexByte(raw, '%'); i >= 0 {
		address, zone = raw[:i], raw[i+1:]
		if zone == "" {
			return "", fmt.Errorf("jetkvm: device URL host is invalid")
		}
	}
	if ip := net.ParseIP(address); ip != nil {
		canonical := ip.String()
		if zone != "" {
			if ip.To4() != nil {
				return "", fmt.Errorf("jetkvm: device URL host is invalid")
			}
			canonical += "%" + zone // interface/zone identifiers can be case-sensitive
		}
		return canonical, nil
	}
	if zone != "" {
		return "", fmt.Errorf("jetkvm: device URL host is invalid")
	}
	host := strings.TrimSuffix(strings.ToLower(raw), ".")
	if host == "" {
		return "", fmt.Errorf("jetkvm: device URL host is invalid")
	}
	return host, nil
}
