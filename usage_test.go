package rosetta

import (
	"context"
	"sync"
	"testing"
)

func TestMemoryUsageTrackerAccumulates(t *testing.T) {
	tr := NewMemoryUsageTracker()
	tr.Record(context.Background(), UsageRecord{
		Protocol: ProtoOpenAIChat, Model: "m1",
		Usage: Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CachedInputTokens: 2, ReasoningTokens: 1},
	})
	tr.Record(context.Background(), UsageRecord{
		Protocol: ProtoAnthropic, Model: "m1",
		Usage:        Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		UsageMissing: true,
	})
	snap := tr.Snapshot()
	if snap.TotalRequests != 2 || snap.InputTokens != 11 || snap.OutputTokens != 6 || snap.TotalTokens != 17 {
		t.Fatalf("totals wrong: %+v", snap)
	}
	if snap.CachedInputTokens != 2 || snap.ReasoningTokens != 1 || snap.UsageMissing != 1 {
		t.Fatalf("details wrong: %+v", snap)
	}
	if m := snap.ByModel["m1"]; m.Requests != 2 {
		t.Fatalf("by-model wrong: %+v", m)
	}
	if p := snap.ByProtocol[ProtoAnthropic]; p.Requests != 1 || p.UsageMissing != 1 {
		t.Fatalf("by-protocol wrong: %+v", p)
	}
}

func TestMemoryUsageTrackerSnapshotIsCopy(t *testing.T) {
	tr := NewMemoryUsageTracker()
	tr.Record(context.Background(), UsageRecord{Model: "m", Usage: Usage{InputTokens: 1}})
	snap := tr.Snapshot()
	snap.ByModel["m"] = ModelUsage{}
	snap.ByModel["injected"] = ModelUsage{Requests: 99}
	again := tr.Snapshot()
	if again.ByModel["m"].Requests != 1 {
		t.Fatal("snapshot mutation leaked into tracker")
	}
	if _, ok := again.ByModel["injected"]; ok {
		t.Fatal("injected key leaked into tracker")
	}
}

func TestMemoryUsageTrackerConcurrent(t *testing.T) {
	tr := NewMemoryUsageTracker()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				tr.Record(context.Background(), UsageRecord{
					Protocol: ProtoOpenAIChat, Model: "m",
					Usage: Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
				})
			}
		}()
	}
	wg.Wait()
	snap := tr.Snapshot()
	if snap.TotalRequests != 800 || snap.TotalTokens != 1600 {
		t.Fatalf("concurrent accounting wrong: %+v", snap)
	}
}

func TestStatsWithoutTracker(t *testing.T) {
	c, err := NewClient(WithEndpoint("http://test"), WithAPIKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	snap := c.Stats()
	if snap.TotalRequests != 0 || snap.ByModel != nil {
		t.Fatalf("expected zero snapshot, got %+v", snap)
	}
}
