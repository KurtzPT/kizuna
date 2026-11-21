package sources

import (
	"sync/atomic"

	"kizuna/internal/fetch"
)

// A recover only covers its own goroutine: a panic on any goroutine this
// package starts would end the whole process, not fail one show build. A
// goroutine a call waits for keeps its panic (caught) for the call to raise
// again once they are done, on the goroutine of the show build, which
// recovers it: as a fetch.GoroutinePanic, with the stack where it started.
// One nobody waits for recovers and logs it (fetch.LogPanic) and leaves its
// state usable.

// caught keeps the first panic of the goroutines a call starts and waits
// for. Each one defers keep; once they are done the call calls raise. The
// zero value is ready.
type caught struct {
	first atomic.Pointer[fetch.GoroutinePanic]
}

// keep, deferred by the goroutine, keeps the panic it is ending with. One
// raised again by a call it made keeps its first stack.
func (c *caught) keep() {
	if r := recover(); r != nil {
		c.first.CompareAndSwap(nil, fetch.AsGoroutinePanic(r))
	}
}

// raise panics with the panic kept, if any. Call it once the goroutines
// are done.
func (c *caught) raise() {
	if p := c.first.Swap(nil); p != nil {
		panic(p)
	}
}
