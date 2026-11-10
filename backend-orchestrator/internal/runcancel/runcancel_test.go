package runcancel

import (
	"sync"
	"testing"
)

func TestCancelReachesEveryRunOfThePipelineOnlyOnce(t *testing.T) {
	var a, b, other int
	unA := Register("p1", func() { a++ })
	unB := Register("p1", func() { b++ })
	unO := Register("p2", func() { other++ })
	defer unO()

	if n := Cancel("p1"); n != 2 {
		t.Fatalf("Cancel reached %d runs, want 2", n)
	}
	if a != 1 || b != 1 || other != 0 {
		t.Fatalf("a=%d b=%d other=%d, want 1 1 0", a, b, other)
	}
	// A cancelled run stays registered until it unwinds; a second Stop must not
	// be counted as reaching a run that is already stopping twice over.
	unA()
	unB()
	if n := Cancel("p1"); n != 0 {
		t.Fatalf("Cancel after unregister reached %d runs, want 0", n)
	}
}

func TestUnregisterIsIdempotentAndCancelOfUnknownIsZero(t *testing.T) {
	un := Register("p1", func() {})
	un()
	un()
	if n := Cancel("nope"); n != 0 {
		t.Fatalf("Cancel(unknown) = %d", n)
	}
	if n := Cancel(""); n != 0 {
		t.Fatalf("Cancel(\"\") = %d", n)
	}
}

func TestRegistryIsSafeUnderConcurrency(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); un := Register("p", func() {}); un() }()
		go func() { defer wg.Done(); Cancel("p") }()
	}
	wg.Wait()
}
