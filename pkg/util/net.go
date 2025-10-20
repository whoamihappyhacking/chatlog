package util

import (
	"net"
	"strings"
)

// isPrivateIPv4 reports whether ip is an RFC1918 private IPv4 address.
func isPrivateIPv4(ip net.IP) bool {
	if ip == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	// 10.0.0.0/8
	if ip4[0] == 10 {
		return true
	}
	// 172.16.0.0/12
	if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
		return true
	}
	// 192.168.0.0/16
	if ip4[0] == 192 && ip4[1] == 168 {
		return true
	}
	return false
}

// LocalIPv4s returns all local IPv4 addresses. If privateOnly is true, only returns RFC1918 addresses.
func LocalIPv4s(privateOnly bool) []string {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		// Skip interfaces that are down or loopback only
		if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			if privateOnly && !isPrivateIPv4(ip4) {
				continue
			}
			ips = append(ips, ip4.String())
		}
	}
	return ips
}

// PrimaryLANIPv4 returns the first private IPv4 address, or empty string if none.
func PrimaryLANIPv4() string {
	ips := LocalIPv4s(true)
	if len(ips) > 0 {
		return ips[0]
	}
	// Fallback to any IPv4 if no private address was found
	ips = LocalIPv4s(false)
	if len(ips) > 0 {
		return ips[0]
	}
	return ""
}

// ComposeLANURL constructs an http URL using the primary LAN IPv4 and the port from addr
// when addr binds all interfaces (0.0.0.0 or ::) or empty host. Otherwise returns http://addr as-is.
func ComposeLANURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// If split fails (e.g., missing port), just return as http URL
		return "http://" + addr
	}
	h := strings.TrimSpace(host)
	if h == "" || h == "0.0.0.0" || h == "::" || h == "[::]" {
		lan := PrimaryLANIPv4()
		if lan != "" {
			return "http://" + lan + ":" + port
		}
	}
	// Not a wildcard bind or no LAN IP found; return the original
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		// IPv6 literal without brackets; add them
		return "http://[" + h + "]:" + port
	}
	return "http://" + h + ":" + port
}
