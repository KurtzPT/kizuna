package sources

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
)

// A recover only covers its own goroutine: a panic on any goroutine this
// package starts would end the whole process, not fail one show build. A
// goroutine a call waits for keeps its panic (caught) for the call to raise
// again once they are done, on the goroutine of the show build, which
// recovers it. One nobody waits for recovers and logs it (logPanic) and
// leaves its state usable.

// goroutinePanic is a panic a goroutine ended with, raised again on the
// goroutine that waited for it, with the stack it had where it started:
// the waiting goroutine's stack says nothing about it.
type goroutinePanic struct {
	value any
	stack []byte
}

// String is the panic's value: what the show build's failure says.
func (p *goroutinePanic) String() string { return fmt.Sprint(p.value) }

// LogValue logs the value with the stack where it started: the show build
// logs only the stack of the goroutine that raised it again.
func (p *goroutinePanic) LogValue() slog.Value {
	return slog.GroupValue(slog.Any("panic", p.value), slog.String("stack", string(p.stack)))
}

// caught keeps the first panic of the goroutines a call starts and waits
// for. Each one defers keep; once they are done the call calls raise. The
// zero value is ready.
type caught struct {
	first atomic.Pointer[goroutinePanic]
}

// keep, deferred by the goroutine, keeps the panic it is ending with. One
// raised again by a call it made keeps its first stack.
func (c *caught) keep() {
	if r := recover(); r != nil {
		p, ok := r.(*goroutinePanic)
		if !ok {
			p = &goroutinePanic{r, debug.Stack()}
		}
		c.first.CompareAndSwap(nil, p)
	}
}

// raise panics with the panic kept, if any. Call it once the goroutines
// are done.
func (c *caught) raise() {
	if p := c.first.Swap(nil); p != nil {
		panic(p)
	}
}

// logPanic logs a panic r recovered on a goroutine nobody waits for, the
// way a show build's panic is logged. Call it from the deferred function
// that recovered r: the stack is then still the one that panicked.
func logPanic(msg string, r any, args ...any) {
	slog.Error(msg, append(args, "err", r, "stack", string(debug.Stack()))...)
}
