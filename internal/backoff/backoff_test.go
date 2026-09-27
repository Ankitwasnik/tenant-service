package backoff

import (
	"context"
	"testing"
	"time"
)

func TestBackoffDoublesToCapAndResets(t *testing.T) {
	b := New(100*time.Millisecond, 400*time.Millisecond)
	// The nominal waits: 100, 200, 400, then capped at 400. Each is jittered
	// into [d/2, d].
	for i, nominal := range []time.Duration{100, 200, 400, 400, 400} {
		nominal *= time.Millisecond
		if got := b.Next(); got < nominal/2 || got > nominal {
			t.Fatalf("wait %d = %v, want within [%v, %v]", i, got, nominal/2, nominal)
		}
	}
	b.Reset()
	if got := b.Next(); got > 100*time.Millisecond {
		t.Fatalf("after Reset: %v, want at most the minimum", got)
	}
}

func TestJitterBounds(t *testing.T) {
	for range 1000 {
		if got := Jitter(time.Second); got < 500*time.Millisecond || got > time.Second {
			t.Fatalf("Jitter(1s) = %v", got)
		}
	}
	if got := Jitter(0); got != 0 {
		t.Fatalf("Jitter(0) = %v", got)
	}
}

func TestSleepStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := Sleep(ctx, time.Hour); err == nil {
		t.Fatal("Sleep returned nil after cancel")
	}
	if time.Since(start) > time.Second {
		t.Fatal("Sleep didn't return promptly on cancel")
	}
	if err := Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
}
