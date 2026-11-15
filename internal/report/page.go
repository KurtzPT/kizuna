package report

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
)

//go:embed page.html
var pageHTML []byte

// dataSlot is replaced by the page's data in page.html.
var dataSlot = []byte("/*KIZUNA_DATA*/null")

// pageHead and pageTail are page.html before and after dataSlot, cut once:
// a long show's page is written often, and the template doesn't change.
var pageHead, pageTail, pageHasSlot = bytes.Cut(pageHTML, dataSlot)

// render returns the HTML file for a page. p.Data must come from
// json.Marshal, which escapes it for a <script> (<, >, & and U+2028/9), so
// no text from a source can end the script or add markup; the page's script
// escapes every value it puts into the document. The data is spliced in as
// it is, not marshaled again: show pages are big.
func render(p page) ([]byte, error) {
	if !pageHasSlot {
		return nil, errors.New("page.html has no data slot")
	}
	if len(p.Data) == 0 {
		return nil, errors.New("page has no data")
	}
	head, err := json.Marshal(p) // {"kind":…,"generated":…,"version":…}
	if err != nil {
		return nil, err
	}
	field, _ := json.Marshal(p.Kind)
	// One buffer for the whole file: the data is copied once.
	var out bytes.Buffer
	out.Grow(len(pageHead) + len(head) + len(field) + len(p.Data) + 2 + len(pageTail))
	out.Write(pageHead)
	out.Write(head[:len(head)-1])
	out.WriteByte(',')
	out.Write(field)
	out.WriteByte(':')
	out.Write(p.Data)
	out.WriteByte('}')
	out.Write(pageTail)
	return out.Bytes(), nil
}
