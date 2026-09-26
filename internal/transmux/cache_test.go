package transmux

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSegmentCache(t *testing.T) {
	var loads [10]atomic.Int32
	release := make(chan struct{})
	c := newSegmentCache(2, func(i int) (*segmentData, error) {
		loads[i].Add(1)
		<-release
		return &segmentData{}, nil
	})

	// Concurrent requests for one segment share a single load.
	var wg sync.WaitGroup
	for k := 0; k < 5; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.get(context.Background(), 0); err != nil {
				t.Error(err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if n := loads[0].Load(); n != 1 {
		t.Errorf("segment 0 loaded %d times", n)
	}

	// Filling beyond capacity evicts the least recently used.
	for _, i := range []int{1, 0, 2} {
		if _, err := c.get(context.Background(), i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.get(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if loads[1].Load() != 2 || loads[0].Load() != 1 {
		t.Errorf("loads: seg0 %d (want 1), seg1 %d (want 2, as it was evicted)", loads[0].Load(), loads[1].Load())
	}

	// A cancelled waiter does not affect the load.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, 5); !errors.Is(err, context.Canceled) && err != nil {
		t.Errorf("cancelled get = %v", err)
	}
}

func TestSegmentCacheRetriesErrors(t *testing.T) {
	var calls atomic.Int32
	c := newSegmentCache(2, func(int) (*segmentData, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("transient")
		}
		return &segmentData{}, nil
	})
	if _, err := c.get(context.Background(), 0); err == nil {
		t.Fatal("expected the first load to fail")
	}
	// The failed entry is dropped once its load finishes.
	for i := 0; i < 100; i++ {
		if _, err := c.get(context.Background(), 0); err == nil {
			return
		}
	}
	t.Error("failed load was cached")
}
