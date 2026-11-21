package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A warning logged before the log file is open (a bad LOG_RETENTION_DAYS,
// read to open it) is held back and handed to the real logger: it reaches
// standard output and the log file, once each, ahead of what comes after.
// main runs in a child process (TestRunMain); with no keys it stops with
// "can't start" before serving.
func TestEarlyLogHandedOver(t *testing.T) {
	dir := t.TempDir()
	// Only a safety net: a child that hangs fails the test instead of
	// hanging it. The start ends long before this.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunMain$", "--", runMainMarker)
	// Nothing from this machine: no keys, no settings file. GOCOVERDIR
	// keeps a coverage build's child from warning on stderr. LISTEN is no
	// address: if a start without keys ever went on, it would stop at the
	// port, before any download.
	cmd.Env = []string{"DATA_DIR=" + dir, "LOG_RETENTION_DAYS=soon", "LISTEN=127.0.0.1:notaport", "GOCOVERDIR=" + t.TempDir()}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("exit: %v, want 1 (no keys); stderr %q", err, stderr.String())
	}
	file, err := os.ReadFile(filepath.Join(dir, "logs", "kizuna.log"))
	if err != nil {
		t.Fatal(err)
	}
	const warning, stop = "setting=LOG_RETENTION_DAYS value=soon", `msg="can't start:`
	for name, out := range map[string]string{"standard output": stdout.String(), "log file": string(file)} {
		w, s := strings.Index(out, warning), strings.Index(out, stop)
		if strings.Count(out, warning) != 1 || s < 0 || w > s {
			t.Errorf("%s: want the early warning once, before the stop:\n%s", name, out)
		}
	}
}
