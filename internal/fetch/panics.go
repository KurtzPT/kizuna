package fetch

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// A recover only covers its own goroutine: a panic on a goroutine a
// package starts would end the whole process, not fail one show build.
// GoroutinePanic and LogPanic are here because every package that starts
// goroutines (sources, provider) imports fetch: one type lets the show
// build tell a goroutine's panic from its own, wherever it started.

// GoroutinePanic is a panic a goroutine ended with, raised again on the
// goroutine that waited for it, with the stack it had where it started:
// the waiting goroutine's stack says nothing about it.
type GoroutinePanic struct {
	Value any
	Stack []byte
}

// AsGoroutinePanic is a recovered value as a GoroutinePanic, with the stack
// of the goroutine recovering it, unless it is one already: it keeps its
// first stack. Call it from the deferred function that recovered r.
func AsGoroutinePanic(r any) *GoroutinePanic {
	if p, ok := r.(*GoroutinePanic); ok {
		return p
	}
	return &GoroutinePanic{r, debug.Stack()}
}

// String is the panic's value: what the show build's failure says.
func (p *GoroutinePanic) String() string { return fmt.Sprint(p.Value) }

// LogValue logs the value with the stack where it started, for a log line
// that logs the panic without taking it apart.
func (p *GoroutinePanic) LogValue() slog.Value {
	return slog.GroupValue(slog.Any("panic", p.Value), slog.String("stack", string(p.Stack)))
}

// LogPanic logs a panic r recovered on a goroutine nobody waits for, the
// way a show build's panic is logged. Call it from the deferred function
// that recovered r: the stack is then still the one that panicked.
func LogPanic(msg string, r any, args ...any) {
	slog.Error(msg, append(args, "err", r, "stack", string(debug.Stack()))...)
}
