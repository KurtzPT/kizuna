package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientAllowed(t *testing.T) {
	private, _ := ParseAccess([]string{"private"}, nil)
	plexOnly, _ := ParseAccess([]string{"192.168.1.10", "10.0.0.0/8"}, nil)
	anyone, _ := ParseAccess([]string{"any"}, nil)
	typo, bad := ParseAccess([]string{"192.168.1.300"}, nil)
	if len(bad) != 1 {
		t.Errorf("typo not reported: %v", bad)
	}
	cases := []struct {
		name   string
		access Access
		remote string
		want   bool
	}{
		{"LAN", private, "192.168.1.20:5000", true},
		{"Docker bridge", private, "172.17.0.1:5000", true},
		{"Tailscale", private, "100.101.102.103:5000", true},
		{"loopback", private, "127.0.0.1:5000", true},
		{"IPv6 ULA", private, "[fd00::5]:5000", true},
		{"IPv6 link-local with zone", private, "[fe80::1%eth0]:5000", true},
		{"IPv4-mapped IPv6 LAN", private, "[::ffff:192.168.1.20]:5000", true},
		{"internet", private, "203.0.113.7:5000", false},
		{"internet IPv6", private, "[2001:db8::1]:5000", false},
		{"Plex server only", plexOnly, "192.168.1.10:5000", true},
		{"loopback always (Docker health check)", plexOnly, "127.0.0.1:5000", true},
		{"IPv6 loopback always", plexOnly, "[::1]:5000", true},
		{"another LAN machine", plexOnly, "192.168.1.11:5000", false},
		{"listed network", plexOnly, "10.1.2.3:5000", true},
		{"any", anyone, "203.0.113.7:5000", true},
		{"typo falls back to private, never to any", typo, "203.0.113.7:5000", false},
		{"typo still allows the LAN", typo, "192.168.1.20:5000", true},
		{"garbage address", private, "not-an-ip", false},
	}
	for _, c := range cases {
		if got := c.access.clientAllowed(c.remote); got != c.want {
			t.Errorf("%s (%s): got %v, want %v", c.name, c.remote, got, c.want)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	def, _ := ParseAccess(nil, nil)
	custom, _ := ParseAccess(nil, []string{"kizuna.example.com", "*.mynas.net"})
	anyHost, _ := ParseAccess(nil, []string{"any"})
	cases := []struct {
		access Access
		host   string
		want   bool
	}{
		{def, "192.168.1.5:32600", true},
		{def, "[fd00::5]:32600", true},
		{def, "localhost:32600", true},
		{def, "kizuna:32600", true}, // Docker container name
		{def, "nas.local:32600", true},
		{def, "nas.lan", true},
		{def, "media.home.arpa:32600", true},
		{def, "", true},                        // HTTP/1.0, no Host
		{def, "evil.example.com:32600", false}, // DNS rebinding
		{def, "kizuna.example.com:32600", false},
		{custom, "kizuna.example.com:32600", true},
		{custom, "KIZUNA.example.com.", true},
		{custom, "plex.mynas.net", true},
		{custom, "mynas.net", true},
		{custom, "evil.example.com", false},
		{custom, "notmynas.net", false},
		{anyHost, "evil.example.com", true},
	}
	for _, c := range cases {
		if got := c.access.hostAllowed(c.host); got != c.want {
			t.Errorf("host %q (%v): got %v, want %v", c.host, c.access.Hosts, got, c.want)
		}
	}
}

func TestGuardRefuses(t *testing.T) {
	access, _ := ParseAccess([]string{"private"}, nil)
	p := &Provider{access: access}
	h := p.guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for _, c := range []struct {
		remote, host string
		want         int
	}{
		{"192.168.1.20:5000", "192.168.1.5:32600", http.StatusOK},
		{"203.0.113.7:5000", "192.168.1.5:32600", http.StatusForbidden},    // from the internet
		{"192.168.1.20:5000", "rebind.attacker.com", http.StatusForbidden}, // DNS rebinding
	} {
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		r.RemoteAddr, r.Host = c.remote, c.host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s -> %s: got %d, want %d", c.remote, c.host, w.Code, c.want)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Error("nosniff header missing")
		}
	}
}
