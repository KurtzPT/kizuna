package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"
)

// showFile is ShowDoc as the store reads and writes it: each build stays the
// JSON it was saved as. A long show's builds are about a megabyte, and most
// updates of its page only add a request, so decoding and encoding them all
// again would be most of the write. Its fields are ShowDoc's, in the same
// order, so both give the same file (TestShowFileIsShowDoc).
type showFile struct {
	Schema int    `json:"schema"`
	Key    string `json:"key"`
	ShowHead
	Builds    []savedBuild `json:"builds"`
	Requests  []Request    `json:"requests"`
	LastMatch *Match       `json:"lastMatch,omitempty"`
}

// savedBuild is one build as saved, and the few values of it the store reads.
type savedBuild struct {
	raw json.RawMessage
	buildHead
}

// buildHead is what showRow and the order of the builds need. Episodes and
// Art are read only to know whether the build still has them.
type buildHead struct {
	ID       string    `json:"id"`
	Root     string    `json:"root"`
	Mode     string    `json:"mode"`
	Start    time.Time `json:"start"`
	Complete bool      `json:"complete"`
	Error    string    `json:"error"`
	Checks   []struct {
		Level Level `json:"level"`
	} `json:"checks"`
	Episodes []struct{} `json:"episodes"`
	Art      []struct{} `json:"art"`
}

func newSavedBuild(b Build) (savedBuild, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return savedBuild{}, err
	}
	var sb savedBuild
	err = sb.UnmarshalJSON(raw)
	return sb, err
}

// UnmarshalJSON keeps a build's bytes as they are when its head reads
// back. One that doesn't (a hand-edited file, a field whose type changed)
// or a null is written again from what decodes, as decoding the whole page
// used to do: kept as it was, it would fail at every later write.
func (b *savedBuild) UnmarshalJSON(data []byte) error {
	*b = savedBuild{}
	if !bytes.Equal(data, []byte("null")) { // null: the zero build, as a []Build gave
		err := json.Unmarshal(data, &b.buildHead)
		if err == nil {
			b.raw = slices.Clone(data) // the decoder may reuse data once this returns
			return nil
		}
		if !partial(err) {
			return err
		}
		slog.Warn("reports: a saved build has values that don't read back; they are dropped", "build", b.ID, "err", err)
	}
	full, err := decodeBuild(data)
	if err != nil {
		return err
	}
	*b, err = newSavedBuild(full)
	return err
}

// decodeBuild decodes a saved build, keeping what decodes when some values
// have the wrong type.
func decodeBuild(data []byte) (Build, error) {
	var full Build
	if err := json.Unmarshal(data, &full); err != nil && !partial(err) {
		return Build{}, err
	}
	return full, nil
}

// partial: Unmarshal kept what decoded and skipped values of the wrong
// type. Only broken JSON leaves nothing.
func partial(err error) bool {
	var syntax *json.SyntaxError
	return err != nil && !errors.As(err, &syntax)
}

// MarshalJSON gives the build back as it was saved. json.Marshal still
// checks it and escapes it for the page's <script>, as for any build.
func (b savedBuild) MarshalJSON() ([]byte, error) { return b.raw, nil }

// lean drops the episode and image lists of a build that is no longer the
// newest: only the newest build's are kept.
func (b *savedBuild) lean() error {
	if len(b.Episodes) == 0 && len(b.Art) == 0 {
		return nil
	}
	// A value of the wrong type outside the head is found only here: it is
	// dropped, as it was when the whole page was decoded.
	var full Build
	if err := json.Unmarshal(b.raw, &full); partial(err) {
		slog.Warn("reports: a saved build has values that don't read back; they are dropped", "build", b.ID, "err", err)
	} else if err != nil {
		return err
	}
	full.Episodes, full.Art = nil, nil
	lean, err := newSavedBuild(full)
	*b = lean
	return err
}
