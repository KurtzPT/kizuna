package sources

import (
	"errors"
	"testing"

	"kizuna/internal/fetch"
)

// episodePages: the first page's error is the list's, a later 404 ends the
// list, any other later error leaves it incomplete; a last page stops it.
func TestEpisodePages(t *testing.T) {
	down := errors.New("down")
	notFound := &fetch.StatusError{Code: 404, URL: "x"}
	cases := []struct {
		name  string
		pages []error // one per page read; nil = a page with another after it
		read  int
		want  func(error) bool
	}{
		{"first page fails", []error{down}, 1, func(err error) bool { return errors.Is(err, down) && !errors.Is(err, ErrIncomplete) }},
		{"first page 404", []error{notFound}, 1, fetch.IsNotFound},
		{"404 past the last page", []error{nil, nil, notFound}, 3, func(err error) bool { return err == nil }},
		{"later page fails", []error{nil, down}, 2, func(err error) bool { return errors.Is(err, ErrIncomplete) }},
	}
	for _, c := range cases {
		read := 0
		err := episodePages(7, 1, func(page int) (bool, error) {
			if page != 1+read {
				t.Errorf("%s: read page %d, want %d", c.name, page, 1+read)
			}
			read++
			return true, c.pages[page-1]
		})
		if read != c.read || !c.want(err) {
			t.Errorf("%s: read %d pages, err %v", c.name, read, err)
		}
	}

	read := 0
	err := episodePages(7, 0, func(page int) (bool, error) { read++; return page < 2, nil })
	if read != 3 || err != nil {
		t.Errorf("last page: read %d pages (want 3), err %v", read, err)
	}
	read = 0
	_ = episodePages(7, 0, func(int) (bool, error) { read++; return true, nil })
	if read != 50 {
		t.Errorf("endless list: read %d pages, want 50", read)
	}
}
