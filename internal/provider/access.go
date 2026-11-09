package provider

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Access control. Plex can't send credentials to a provider, so Kizuna
// limits who can talk to it instead:
//
//   - ALLOW_FROM: the client's address must be in an allowed network (by
//     default the private ranges), so an accidental port forward to the
//     internet answers nothing. The connection's own address is used, never
//     X-Forwarded-For, which a client can fake.
//   - ALLOWED_HOSTS: the Host the request was sent to must be an IP address,
//     a local name or a listed name. A web page using DNS rebinding always
//     sends its own domain, so it can't make your browser talk to Kizuna.

// privateNets is what ALLOW_FROM=private means: loopback, the private IPv4
// ranges, shared address space (Tailscale, carrier NAT), link-local, and the
// IPv6 equivalents.
var privateNets = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

// localSuffixes: host names under these are local by convention and always
// accepted (besides IP addresses, "localhost" and one-word names).
var localSuffixes = []string{".local", ".lan", ".home", ".home.arpa", ".internal", ".localdomain"}

// Access is the parsed ALLOW_FROM / ALLOWED_HOSTS.
type Access struct {
	AnyClient bool           // ALLOW_FROM=any
	Nets      []netip.Prefix // allowed client networks
	AnyHost   bool           // ALLOWED_HOSTS=any (or *)
	Hosts     []string       // extra host names; ".example.com" = it and every name under it
}

// ParseAccess reads the two settings. Bad entries are returned in bad, so
// the caller can warn about them. An ALLOW_FROM without one valid entry
// falls back to "private": a typo must never open Kizuna to everyone.
func ParseAccess(allowFrom, allowedHosts []string) (a Access, bad []string) {
	for _, e := range allowFrom {
		switch v := strings.ToLower(strings.TrimSpace(e)); {
		case v == "any" || v == "all":
			a.AnyClient = true
		case v == "private":
			a.Nets = append(a.Nets, privateNets...)
		default:
			if p, err := netip.ParsePrefix(v); err == nil {
				a.Nets = append(a.Nets, unmapPrefix(p))
			} else if ip, err := netip.ParseAddr(v); err == nil {
				a.Nets = append(a.Nets, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
			} else if v != "" {
				bad = append(bad, "ALLOW_FROM="+e)
			}
		}
	}
	if !a.AnyClient && len(a.Nets) == 0 {
		a.Nets = privateNets
	}
	for _, e := range allowedHosts {
		// a trailing dot is dropped, as from the Host (hostAllowed)
		v := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(e)), ".")
		if v != "*" {
			v = strings.TrimPrefix(v, "*") // "*.example.com" = ".example.com"
		}
		switch {
		case v == "any" || v == "all" || v == "*":
			a.AnyHost = true
		case v == "" || strings.ContainsAny(v, "/: "):
			if v != "" {
				bad = append(bad, "ALLOWED_HOSTS="+e)
			}
		default:
			a.Hosts = append(a.Hosts, v)
		}
	}
	return a, bad
}

// unmapPrefix masks an ALLOW_FROM network and turns an IPv4-mapped one
// (::ffff:192.168.1.0/120) into its IPv4 network (192.168.1.0/24), as
// clientAllowed unmaps the client's address: written either way, it matches.
func unmapPrefix(p netip.Prefix) netip.Prefix {
	p = p.Masked()
	if ip := p.Addr(); ip.Is4In6() && p.Bits() >= 96 {
		return netip.PrefixFrom(ip.Unmap(), p.Bits()-96)
	}
	return p
}

// clientAllowed reports whether a connection's remote address may use Kizuna.
// Loopback (127.0.0.1, ::1) is always allowed, whatever ALLOW_FROM says.
func (a Access) clientAllowed(remoteAddr string) bool {
	if a.AnyClient {
		return true
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap().WithZone("")
	if ip.IsLoopback() { // the same machine or container: Docker's health check, the migration script on the host
		return true
	}
	for _, n := range a.Nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// fromSelf reports whether a request came from the machine (or container)
// Kizuna runs on, over one of its own addresses: the connection's remote IP
// is the IP it arrived on. Docker's health check with LISTEN=<ip> connects
// this way, not over loopback. A remote client can't make such a
// connection: the answers to a faked source address would never reach it.
func fromSelf(r *http.Request) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	lhost, _, err := net.SplitHostPort(local.String())
	if err != nil {
		return false
	}
	rhost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	l, err1 := netip.ParseAddr(lhost)
	rm, err2 := netip.ParseAddr(rhost)
	return err1 == nil && err2 == nil && l.Unmap().WithZone("") == rm.Unmap().WithZone("")
}

// hostAllowed reports whether a request's Host header names Kizuna in a way
// a DNS rebinding page can't: an IP address, localhost, a one-word name
// (a container name such as "kizuna"), a local suffix, or ALLOWED_HOSTS.
func (a Access) hostAllowed(hostHeader string) bool {
	if a.AnyHost || hostHeader == "" { // no Host: HTTP/1.0 clients, never a browser
		return true
	}
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, s := range localSuffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	for _, h := range a.Hosts {
		if host == h || (strings.HasPrefix(h, ".") && (strings.HasSuffix(host, h) || host == h[1:])) {
			return true
		}
	}
	return false
}

// guard refuses requests from outside ALLOW_FROM or for a host name outside
// ALLOWED_HOSTS with 403, and logs each refused client or host name once
// every 10 minutes (an internet scan must not flood the log).
func (p *Provider) guard(next http.Handler) http.Handler {
	refused := &everyTenMinutes{max: 1000}
	warnOnce := func(key, msg string, attrs ...any) {
		if refused.due(key) {
			slog.Warn(msg, attrs...)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// /health only says ok, starting or degraded: the machine itself may
		// always ask, over any of its addresses (Docker's health check).
		if !p.access.clientAllowed(r.RemoteAddr) && !(r.URL.Path == "/health" && fromSelf(r)) {
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			warnOnce("ip "+ip, "refused a request from an address outside ALLOW_FROM; if it's yours, add it to ALLOW_FROM",
				"client", ip, "url", r.URL.Path)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "this address may not use Kizuna (ALLOW_FROM)"})
			return
		}
		if !p.access.hostAllowed(r.Host) {
			warnOnce("host "+r.Host, "refused a request for an unknown host name (DNS rebinding protection); "+
				"if you reach Kizuna by this name, add it to ALLOWED_HOSTS", "host", r.Host, "client", r.RemoteAddr)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "unknown host name (ALLOWED_HOSTS)"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
