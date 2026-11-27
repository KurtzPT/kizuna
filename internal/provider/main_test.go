package provider

import (
	"testing"

	"kizuna/internal/fetch"
)

// No file these tests write is read back after a crash, and on macOS each
// sync also flushes the drive's cache: with it the tests take several
// times as long.
func TestMain(m *testing.M) {
	fetch.SkipSyncInTests()
	m.Run()
}
