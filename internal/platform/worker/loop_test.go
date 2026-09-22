package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoopRunsUntilCancel(t *testing.T) {
	var ticks atomic.Int64
	loop := NewLoop(10*time.Millisecond, func(context.Context) error {
		ticks.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = loop.Run(ctx); close(done) }()

	time.Sleep(55 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop after cancel")
	}
	if got := ticks.Load(); got < 2 {
		t.Fatalf("ticks = %d, want >= 2", got)
	}
}

func TestLoopSurvivesPanic(t *testing.T) {
	var ticks, panics atomic.Int64
	loop := NewLoop(5*time.Millisecond, func(context.Context) error {
		if ticks.Add(1) == 1 {
			panics.Add(1)
			panic("boom")
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = loop.Run(ctx); close(done) }()

	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	if panics.Load() != 1 {
		t.Fatalf("panics = %d, want 1", panics.Load())
	}
	if ticks.Load() < 2 {
		t.Fatalf("loop died after panic: ticks = %d", ticks.Load())
	}
}

func TestLoopContinuesAfterError(t *testing.T) {
	var ticks atomic.Int64
	loop := NewLoop(5*time.Millisecond, func(context.Context) error {
		ticks.Add(1)
		return errors.New("transient")
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = loop.Run(ctx); close(done) }()

	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	if ticks.Load() < 2 {
		t.Fatalf("ticks = %d, want >= 2 (errors must not stop the loop)", ticks.Load())
	}
}
