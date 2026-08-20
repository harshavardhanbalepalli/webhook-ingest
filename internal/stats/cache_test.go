package stats_test

import (
	"github.com/convin/webhook-ingest/internal/stats"
	"sync"
	"testing"
)

func TestCacheRecordIsSafeForConcurrentUse(t *testing.T) {
	c := stats.NewCache()

	var wg sync.WaitGroup
	const writers = 100
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			c.Record("acc_concurrent", 1)
		}()
	}
	wg.Wait()

	got := c.Get("acc_concurrent")
	if got.CallCount != writers {
		t.Fatalf("got CallCount=%d, want %d", got.CallCount, writers)
	}
}

func TestCacheGetUnknownAccountIsZero(t *testing.T) {
	c := stats.NewCache()
	if got := c.Get("nobody"); got.CallCount != 0 || got.TotalDurationSec != 0 {
		t.Fatalf("got %+v, want zero value", got)
	}
}
