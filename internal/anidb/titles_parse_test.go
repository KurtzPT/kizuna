package anidb

import (
	"reflect"
	"testing"
	"unsafe"
)

// The parsed titles keep their values, and titles with the same type or
// language share one copy of it: the list holds some 100k titles.
func TestParseTitlesSharesTypeAndLang(t *testing.T) {
	byAID, err := parseTitles([]byte(`<animetitles>
<anime aid="1"><title type="main" xml:lang="x-jat">Cowboy Bebop</title><title type="official" xml:lang="en">Cowboy Bebop</title></anime>
<anime aid="2"><title type="main" xml:lang="x-jat">Trigun</title><title type="official" xml:lang="en">Trigun</title><title type="syn" xml:lang="zh-Hans">枪神</title></anime>
</animetitles>`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][]Title{
		1: {{"main", "x-jat", "Cowboy Bebop"}, {"official", "en", "Cowboy Bebop"}},
		2: {{"main", "x-jat", "Trigun"}, {"official", "en", "Trigun"}, {"syn", "zh-Hans", "枪神"}},
	}
	if !reflect.DeepEqual(byAID, want) {
		t.Fatalf("parsed %v, want %v", byAID, want)
	}
	same := func(a, b string) bool { return unsafe.StringData(a) == unsafe.StringData(b) }
	a, b := byAID[1], byAID[2]
	if !same(a[0].Type, b[0].Type) || !same(a[0].Lang, b[0].Lang) || !same(a[1].Type, b[1].Type) || !same(a[1].Lang, b[1].Lang) {
		t.Error("the same type or language is a copy of its own per title")
	}
}
