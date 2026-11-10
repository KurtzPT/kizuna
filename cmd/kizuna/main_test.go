package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

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
		":32600":               "http://127.0.0.1:32600/health",
		"0.0.0.0:32600":        "http://127.0.0.1:32600/health",
		"[::]:32600":           "http://127.0.0.1:32600/health",
		"192.168.1.5:32600":    "http://192.168.1.5:32600/health",
		"[fd00::5]:32600":      "http://[fd00::5]:32600/health",
		"127.0.0.1:32600":      "http://127.0.0.1:32600/health",
		"kizuna.lan:32600":     "http://kizuna.lan:32600/health",
		"[fe80::1%eth0]:32600": "http://[fe80::1%25eth0]:32600/health",
	} {
		got := healthURL(listen)
		if got != want {
			t.Errorf("LISTEN=%s: %s, want %s", listen, got, want)
		}
		if _, err := http.NewRequest(http.MethodGet, got, nil); err != nil {
			t.Errorf("LISTEN=%s: %v", listen, err)
		}
	}
}

// The health check ignores HTTP_PROXY: through a proxy, LISTEN=<ip> would be
// asked from the proxy's address and refused (or not reached).
func TestHealthClientNoProxy(t *testing.T) {
	if tr, ok := healthClient.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Error("the health client uses the environment's proxy")
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
	direct := &http.Client{Transport: &http.Transport{Proxy: nil}} // never through the machine's HTTP_PROXY
	defer direct.CloseIdleConnections()
	resp, err := direct.Get("http://" + ln.Addr().String() + "/anidb/library/metadata/anidb-1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("metadata from the machine's own address: HTTP %d, want 403 (ALLOW_FROM)", resp.StatusCode)
	}
}

// Stopping during the first load doesn't count as loaded ("ready" isn't
// logged); a finished first load does, and stopping later ends the loop.
func TestKeepFreshStopping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := false
	keepFresh(ctx, cancel, func() (bool, []string) { return true, nil }, func() { done = true })
	if done {
		t.Error("load cancelled by stopping, but firstDone ran")
	}

	ctx, cancel = context.WithCancel(context.Background())
	loads := 0
	keepFresh(ctx, func() { loads++ }, func() (bool, []string) { return true, nil }, func() { done = true; cancel() })
	if !done || loads != 1 {
		t.Errorf("firstDone %v after %d loads, want it after 1", done, loads)
	}
}

// keepFresh retries in 5 minutes while the titles or a mapping list have
// never loaded (one list alone makes the mapping Ready, but lacks the other's
// ids), and waits a day once everything has.
func TestKeepFreshRetriesMissingList(t *testing.T) {
	old := after
	t.Cleanup(func() { after = old })
	for _, c := range []struct {
		titles  bool
		missing []string
		want    time.Duration
	}{
		{true, nil, 24 * time.Hour},
		{true, []string{"animelists"}, 5 * time.Minute},
		{true, []string{"animelists", "fribb"}, 5 * time.Minute},
		{false, nil, 5 * time.Minute},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		var waits []time.Duration
		after = func(d time.Duration) <-chan time.Time {
			waits = append(waits, d)
			cancel() // stop after the first wait is chosen
			return nil
		}
		keepFresh(ctx, func() {}, func() (bool, []string) { return c.titles, c.missing }, func() {})
		cancel()
		if len(waits) != 1 || waits[0] != c.want {
			t.Errorf("titles %v, missing %v: waits %v, want [%v]", c.titles, c.missing, waits, c.want)
		}
	}
}

// A load cancelled by stopping isn't logged as an error; other failures are.
func TestLoadErrQuietWhenStopping(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	ctx, cancel := context.WithCancel(context.Background())
	loadErr(ctx, "running", errors.New("boom"))
	cancel()
	loadErr(ctx, "stopping", context.Canceled)
	if log := buf.String(); !strings.Contains(log, "running") || strings.Contains(log, "stopping") {
		t.Errorf("log %q: want only the error before stopping", log)
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

// Stopping waits for the show builds that outlived their requests, after
// the server's requests and within the same drain time, so their match
// reports are queued before serve's last flush; builds still running when
// that time is up are logged and left.
func TestDrainWaitsForBuilds(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	waiting, release := make(chan context.Context), make(chan bool)
	drained := make(chan struct{})
	go func() {
		drain(&http.Server{}, func(ctx context.Context) bool { waiting <- ctx; return <-release }, drainTimeout)
		close(drained)
	}()
	ctx := <-waiting // the server had no requests to wait for
	if d, ok := ctx.Deadline(); !ok || time.Until(d) > drainTimeout {
		t.Errorf("builds waited for until %v (deadline %v), want within the drain", d, ok)
	}
	select {
	case <-drained:
		t.Fatal("the drain ended before the builds")
	default:
	}
	release <- true
	<-drained
	if strings.Contains(logs.String(), "build still running") {
		t.Errorf("builds ended in time, logs:\n%s\nwant no warning", logs.String())
	}

	drain(&http.Server{}, func(context.Context) bool { return false }, drainTimeout) // out of time
	if !strings.Contains(logs.String(), "level=WARN msg=\"stopping with a show build still running") {
		t.Errorf("builds still running after the drain, logs:\n%s\nwant a warning", logs.String())
	}
}
