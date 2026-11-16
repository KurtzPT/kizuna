package provider

import (
	"sync"
	"sync/atomic"

	"kizuna/internal/fetch"
)

// group is the sync.WaitGroup of a show build's goroutines. A recover only
// covers its own goroutine, so a panic in one of them would end the whole
// process, not fail one show. Go recovers it there, and Wait panics with it
// on the goroutine that waits: it ends in runBuild's recover as the build's
// failure. The zero value is ready.
//
// A Wait that panics skips what follows it, so every group a build starts
// is also waited for in a defer: no goroutine outlives its build.
type group struct {
	wg sync.WaitGroup
	// first: the first panic of a goroutine, until Wait takes it. The type is
	// fetch's, shared with sources: runBuild takes either apart the same way.
	first atomic.Pointer[fetch.GoroutinePanic]
}

// Go runs f in a goroutine of its own. It allocates what sync.WaitGroup.Go
// does (the budgets of TestReportsOffAllocs).
func (g *group) Go(f func()) {
	g.wg.Add(1)
	go g.run(f)
}

func (g *group) run(f func()) {
	defer g.wg.Done()
	defer g.keepPanic()
	f()
}

// fetcher is a value a group fetches ahead (lazy.start).
type fetcher interface{ fetch() }

// goFetch runs f.fetch like Go, without the closure a method value would
// allocate (the budgets of TestReportsOffAllocs).
func (g *group) goFetch(f fetcher) {
	g.wg.Add(1)
	go g.runFetch(f)
}

func (g *group) runFetch(f fetcher) {
	defer g.wg.Done()
	defer g.keepPanic()
	f.fetch()
}

// keepPanic keeps the panic the goroutine is ending with, for Wait. One
// that came from another group's Wait keeps its first stack.
func (g *group) keepPanic() {
	if r := recover(); r != nil {
		g.first.CompareAndSwap(nil, fetch.AsGoroutinePanic(r))
	}
}

// keepStack, deferred in a function whose result several goroutines share (a
// sync.OnceValue), sends its panic on with the stack it had here. The once
// panics with the same value for every later caller, so whichever group is
// waited last, the build's log shows where the panic started.
func keepStack() {
	if r := recover(); r != nil {
		panic(fetch.AsGoroutinePanic(r))
	}
}

// Wait waits for every goroutine started so far, then panics with the first
// panic one of them ended with (once: a later Wait finds none).
func (g *group) Wait() {
	g.wg.Wait()
	if p := g.first.Swap(nil); p != nil {
		panic(p)
	}
}
