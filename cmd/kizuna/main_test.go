package main

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"kizuna/internal/anidb"
	"kizuna/internal/config"
	"kizuna/internal/mapping"
	"kizuna/internal/provider"
)

func TestListenPortInUse(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	_, err = listen(taken.Addr().String())
	if err == nil {
		t.Fatal("second listen on the same port succeeded")
	}
	_, port, _ := net.SplitHostPort(taken.Addr().String())
	if msg := err.Error(); !strings.Contains(msg, "port "+port+" is already in use") || !strings.Contains(msg, "PORT") {
		t.Errorf("unclear error: %s", msg)
	}

	ln, err := listen("127.0.0.1:0") // a free port works
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}

func TestHealthURL(t *testing.T) {
	for listen, want := range map[string]string{
		":32600":            "http://127.0.0.1:32600/health",
		"0.0.0.0:32600":     "http://127.0.0.1:32600/health",
		"[::]:32600":        "http://127.0.0.1:32600/health",
		"192.168.1.5:32600": "http://192.168.1.5:32600/health",
		"[fd00::5]:32600":   "http://[fd00::5]:32600/health",
		"127.0.0.1:32600":   "http://127.0.0.1:32600/health",
		"kizuna.lan:32600":  "http://kizuna.lan:32600/health",
	} {
		if got := healthURL(listen); got != want {
			t.Errorf("LISTEN=%s: %s, want %s", listen, got, want)
		}
	}
}

// The health check against a real server bound to one of this machine's
// non-loopback addresses (LISTEN=<ip>) with a narrowed ALLOW_FROM: /health
// answers it, a metadata request from the same address is still refused.
func TestHealthCheckWithListenIPAndNarrowAllowFrom(t *testing.T) {
	ip := ownAddress(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Skip("can't listen on", ip, err)
	}
	titles, lists := &anidb.Titles{}, &mapping.Lists{}
	titles.SetForTest(map[int][]anidb.Title{1: {{Type: "main", Text: "A"}}})
	lists.SetForTest(map[int]mapping.Entry{1: {AID: 1}})
	cfg := config.Config{Listen: ln.Addr().String(), AllowFrom: []string{"203.0.113.9"}}
	p := provider.New(cfg, titles, nil, lists, nil, nil, nil, nil)
	srv := &http.Server{Handler: p.Handler()}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	if code := health(cfg); code != 0 {
		t.Errorf("health check: exit %d, want 0", code)
	}
	resp, err := http.Get("http://" + ln.Addr().String() + "/anidb/library/metadata/anidb-1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("metadata from the machine's own address: HTTP %d, want 403 (ALLOW_FROM)", resp.StatusCode)
	}
}

// ownAddress returns one of this machine's non-loopback IPv4 addresses.
func ownAddress(t *testing.T) netip.Addr {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				return ip.Unmap()
			}
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return netip.Addr{}
}
