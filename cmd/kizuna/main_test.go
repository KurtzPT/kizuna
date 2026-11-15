package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
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
	p := provider.New(cfg, titles, nil, lists, provider.Sources{})
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
	keepFresh(ctx, cancel, func() (bool, []string) { return true, nil }, func(bool) { done = true })
	if done {
		t.Error("load cancelled by stopping, but firstDone ran")
	}

	ctx, cancel = context.WithCancel(context.Background())
	loads := 0
	keepFresh(ctx, func() { loads++ }, func() (bool, []string) { return true, nil }, func(bool) { done = true; cancel() })
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
		keepFresh(ctx, func() {}, func() (bool, []string) { return c.titles, c.missing }, func(bool) {})
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

// firstDone hears whether the first load got the title list, so the line
// it logs can say so.
func TestKeepFreshTellsFirstDoneTheTitles(t *testing.T) {
	for _, titles := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		var got []bool
		keepFresh(ctx, func() {}, func() (bool, []string) { return titles, nil }, func(b bool) { got = append(got, b); cancel() })
		cancel()
		if len(got) != 1 || got[0] != titles {
			t.Errorf("titles loaded %v: firstDone got %v, want [%v]", titles, got, titles)
		}
	}
}

// Without the title list nothing matches by name, so the settings line is
// a warning saying so and when it's retried, not "ready".
func TestReadyLineWithoutTitles(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	logReady(config.Config{}, ln, nil, time.Now(), false, true)
	if line := logs.String(); !strings.Contains(line, " level=INFO msg=ready ") {
		t.Errorf("titles loaded: logged %q, want an INFO ready line", line)
	}
	logs.Reset()
	logReady(config.Config{}, ln, nil, time.Now(), false, false)
	line := logs.String()
	if strings.Contains(line, "msg=ready") || !strings.Contains(line, "level=WARN") ||
		!strings.Contains(line, "title list") || !strings.Contains(line, retryMissing.String()) || !strings.Contains(line, "version=") {
		t.Errorf("titles missing: logged %q, want a WARN naming the title list and the retry, with the settings", line)
	}
}

// fixedListener is a listener whose address doesn't change between runs.
type fixedListener struct{ net.Listener }

func (fixedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 32600} }

// The ready line, whole, for settings read by config.Load: the same keys in
// the same order, each list as it works (sources without a key skipped).
func TestReadyLineSettings(t *testing.T) {
	for k, v := range map[string]string{
		"ALLOW_FROM": "private,10.1.0.0/16", "ALLOWED_HOSTS": "kizuna.lan", "PLEX_TOKEN": "token",
		"ANIDB_CLIENT": "client", "TVDB_API_KEY": "", "TVDB_LEGACY_API_KEY": "", "TMDB_API_KEY": "key", "MAL_CLIENT_ID": "",
		"TITLE_SOURCES": "none", "SUMMARY_SOURCES": "", "POSTER_SOURCES": "mal,tmdb,anidb", "BACKGROUND_SOURCES": "",
		"LOGO_SOURCES": "plex", "EPISODE_FILL_SOURCES": "", "EPISODE_CREW_SOURCES": "plex,tvdb", "EPISODE_TITLES": "",
		"GENRE_SOURCES": "", "CAST_SOURCES": "tvdb,plex,ann", "STUDIO_SOURCES": "", "CREW_SOURCES": "", "RATING_SOURCES": "",
		"DATE_SOURCES": "ann,anidb", "ART_ALL_SOURCES": "true", "SEASON_ART_FALLBACK": "", "CAST_PHOTOS_FROM_PLEX": "true", "CREW_PHOTOS_FROM_PLEX": "",
		"RATING_COUNTRY": "de,us", "AUTO_DEFAULT": "tvdb", "CACHE_MIN_FREE": "3GB", "CACHE_FREE_TARGET": "2GB", "MATCH_REPORTS": "true",
	} {
		t.Setenv(k, v)
	}
	cfg := config.Load()
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey || a.Key == "startup" {
			return slog.Attr{}
		}
		return a
	}})))
	defer slog.SetDefault(old)

	logReady(cfg, fixedListener{}, nil, time.Now(), true, true)
	want := "level=INFO msg=ready version=" + provider.Version + " listen=127.0.0.1:32600 allow_from=\"[private 10.1.0.0/16]\" " +
		"allowed_hosts=[kizuna.lan] tvdb=off tmdb=true mal=false plex=true anidb_client=true " +
		"titles=[] summaries=\"[anidb tmdb plex ann]\" posters=\"[tmdb anidb]\" backgrounds=\"[tmdb plex]\" logos=[plex] " +
		"art_all=true season_art_fallback=true genres=\"[anidb tmdb plex ann]\" cast=\"[plex ann]\" cast_photos_from_plex=true " +
		"studio=\"[anidb tmdb plex ann]\" crew=\"[anidb tmdb plex ann]\" rating=\"[anidb tmdb plex ann]\" rating_country=\"[DE US]\" " +
		"date=\"[ann anidb]\" episode_fill=[plex] episode_crew=[plex] crew_photos_from_plex=false " +
		"episode_titles=\"[anidb tmdb plex]\" auto_default=tvdb cache_min_free_mb=3072 cache_free_target_mb=3072 " +
		"match_reports=true extra_titles=true\n"
	if got := logs.String(); got != want {
		t.Errorf("ready line\n got %q\nwant %q", got, want)
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

// The opening and ending titles need PLEX_URL and PLEX_TOKEN, both and
// usable: anything less is off, with a warning that never shows the token.
// PLEX_TOKEN alone is a valid setup (the plex source uses it): one info
// line says the titles need PLEX_URL too. PLEX_TLS_NAME needs an https://
// PLEX_URL and one host name; without PLEX_URL it is a warning, never a
// silent no-op (without PLEX_TOKEN, PLEX_URL's own warning covers it).
func TestExtraTitles(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	for _, c := range []struct {
		url, token, name string
		on               bool
		log              string // the level logged, "" = nothing
	}{
		{"", "", "", false, ""},
		{"http://plex:32400", "s3cret", "", true, ""},
		{"http://plex:32400", "", "", false, "WARN"},
		{"", "s3cret", "", false, "INFO"},
		{"plex:32400", "s3cret", "", false, "WARN"},
		{"http://plex:32400/web", "s3cret", "", false, "WARN"},
		{"http://user:s3cret@plex:32400", "s3cret", "", false, "WARN"},
		{"https://192.168.1.20:32400", "s3cret", "plex.example.com", true, ""},
		{"http://192.168.1.20:32400", "s3cret", "plex.example.com", false, "WARN"},
		{"https://192.168.1.20:32400", "s3cret", "https://plex.example.com", false, "WARN"},
		{"https://192.168.1.20:32400", "s3cret", "192.168.1.20", false, "WARN"},
		{"https://s3cret@192.168.1.20:32400", "s3cret", "plex.example.com", false, "WARN"}, // PLEX_URL's own warning, once
		{"", "", "plex.example.com", false, "WARN"},
		{"", "s3cret", "plex.example.com", false, "WARN"},
		{"https://192.168.1.20:32400", "", "plex.example.com", false, "WARN"},
	} {
		logs.Reset()
		r := extraTitles(config.Config{PlexURL: c.url, PlexToken: c.token, PlexTLSName: c.name})
		lines := strings.Count(logs.String(), "\n")
		if (r != nil) != c.on || strings.Contains(logs.String(), "s3cret") || strings.Contains(logs.String(), "192.168.1.20:32400") ||
			c.log == "" && lines != 0 || c.log != "" && (lines != 1 || !strings.Contains(logs.String(), "level="+c.log)) {
			t.Errorf("PLEX_URL=%q PLEX_TOKEN=%q PLEX_TLS_NAME=%q: on=%v, log %q", c.url, c.token, c.name, r != nil, logs.String())
		}
		if c.url == "" && c.name != "" && !strings.Contains(logs.String(), "PLEX_TLS_NAME is ignored") {
			t.Errorf("PLEX_TLS_NAME without PLEX_URL (PLEX_TOKEN=%q): log %q, want it named as ignored", c.token, logs.String())
		}
	}
}

// runMainMarker after "--" makes TestRunMain run main with the arguments
// that follow it, in a child process.
const runMainMarker = "-kizuna-run-main"

// TestRunMain is main in a child process (started by
// TestArguments); in a normal test run it does nothing.
func TestRunMain(t *testing.T) {
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) && os.Args[i+1] == runMainMarker {
			os.Args = append([]string{"kizuna"}, os.Args[i+2:]...)
			main()
			os.Exit(0) // main returned: the provider would be running
		}
	}
}

// Only "health" or nothing runs: a typo or an extra argument prints the
// usage to stderr and exits 2 before anything starts (no log folder in the
// data folder), and -h / --help print it to stdout with exit 0. Without
// keys, a start that went on would exit 1.
func TestArguments(t *testing.T) {
	for _, c := range []struct {
		args         []string
		exit         int
		stdout, errs string
	}{
		{[]string{"helath"}, 2, "", "kizuna: unknown argument\n" + usage},
		{[]string{"health", "now"}, 2, "", "kizuna: unknown argument\n" + usage},
		{[]string{"--port=1"}, 2, "", "kizuna: unknown argument\n" + usage},
		{[]string{"--help"}, 0, usage, ""},
		{[]string{"-h"}, 0, usage, ""},
	} {
		dir := t.TempDir()
		// Only a safety net: a child that hangs fails the test instead of
		// hanging it. Every case ends long before this.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestRunMain$", "--", runMainMarker}, c.args...)...)
		// Nothing from this machine: no keys, no settings file. GOCOVERDIR
		// keeps a coverage build's child from warning on stderr.
		cmd.Env = []string{"DATA_DIR=" + dir, "GOCOVERDIR=" + t.TempDir()}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		exit := 0
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		_, logsErr := os.Stat(filepath.Join(dir, "logs"))
		if exit != c.exit || stdout.String() != c.stdout || stderr.String() != c.errs || !errors.Is(logsErr, fs.ErrNotExist) {
			t.Errorf("kizuna %v: exit %d, stdout %q, stderr %q, logs folder: %v; want exit %d, stdout %q, stderr %q, no logs folder",
				c.args, exit, stdout.String(), stderr.String(), logsErr, c.exit, c.stdout, c.errs)
		}
	}
}

// Kizuna logs what Plex sends as it came: a title, a path or a body may
// hold a line break. Whatever part of a log line it lands in, the line
// stays one line, so no entry can be forged (CodeQL's go/log-injection).
func TestLogLinesCantBeForged(t *testing.T) {
	const forged = "time=2026-01-01T00:00:00Z level=ERROR msg=forged"
	for _, br := range []string{"\n", "\r\n", "\r"} {
		var out bytes.Buffer
		bad := "x" + br + forged
		slog.New(newLogHandler(&out, slog.LevelInfo)).Info("message "+bad, "key "+bad, "value "+bad, "err", errors.New(bad), "list", []string{bad})
		line, rest, _ := strings.Cut(out.String(), "\n")
		if rest != "" || strings.ContainsAny(line, "\r\n") || !strings.HasPrefix(line, "time=") || strings.Count(line, forged) != 5 {
			t.Errorf("break %q: logged %q, want one line holding all five texts", br, out.String())
		}
	}
}
