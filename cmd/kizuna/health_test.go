package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kizuna/internal/config"
)

// healthStderr runs health with os.Stderr sent to a file, and returns the
// exit code and what it wrote there.
func healthStderr(t *testing.T, cfg config.Config) (int, string) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stderr
	t.Cleanup(func() { os.Stderr = old }) // also when health panics
	os.Stderr = f
	code := health(cfg)
	os.Stderr = old
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, string(b)
}

// The health check fails (exit 1) when nothing answers on the port (here a
// listener that hangs up on every caller: a closed port could be taken by
// another process before the check), and when /health answers anything but
// 200. Docker shows what it wrote on
// stderr next to "unhealthy": the status and Kizuna's reason.
func TestHealthFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // the listener is closed: the test is over
			}
			c.Close()
		}
	}()
	down := ln.Addr().String()
	if code, errs := healthStderr(t, config.Config{Listen: down}); code != 1 || !strings.HasPrefix(errs, "kizuna health: ") {
		t.Errorf("server down: exit %d, stderr %q; want 1 and the error", code, errs)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"degraded","missing":["mapping"],"version":"dev"}` + "\n"))
	}))
	defer srv.Close()
	code, errs := healthStderr(t, config.Config{Listen: srv.Listener.Addr().String()})
	if want := `kizuna health: HTTP 503 {"status":"degraded","missing":["mapping"],"version":"dev"}` + "\n"; code != 1 || errs != want {
		t.Errorf("degraded: exit %d, stderr %q; want 1 and %q", code, errs, want)
	}
}
