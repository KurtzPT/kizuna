//go:build race

package provider

// raceEnabled: the race detector allocates on its own, so allocation counts
// mean nothing under -race.
const raceEnabled = true
