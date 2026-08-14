package executor

import (
	"context"
	"sort"
	"testing"
	"time"
)

// TestLatencyProfile is the headline measurement: container create -> code
// executing, sampled over N sequential runs. Not a Go benchmark because we
// want percentiles, not ns/op.
//
//	go test ./internal/executor -run TestLatencyProfile -v -count=1
func TestLatencyProfile(t *testing.T) {
	d := newTestExecutor(t)
	const samples = 30

	var startup, total []int64
	for i := 0; i < samples; i++ {
		out := make(chan OutputChunk, 64)
		go func() {
			for range out {
			}
		}()
		res, err := d.Execute(context.Background(), ExecRequest{
			Language: "python",
			Code:     `print("x")`,
			Limits:   DefaultLimits(),
			Timeout:  30 * time.Second,
		}, out)
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		startup = append(startup, res.StartupMS)
		total = append(total, res.StartupMS+res.DurationMS)
	}

	t.Logf("n=%d", samples)
	t.Logf("startup  p50=%dms  p90=%dms  p99=%dms  min=%dms  max=%dms",
		pct(startup, 50), pct(startup, 90), pct(startup, 99), pct(startup, 0), pct(startup, 100))
	t.Logf("total    p50=%dms  p90=%dms  p99=%dms",
		pct(total, 50), pct(total, 90), pct(total, 99))
}

func pct(vals []int64, p int) int64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]int64(nil), vals...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if p <= 0 {
		return s[0]
	}
	if p >= 100 {
		return s[len(s)-1]
	}
	idx := (len(s)*p + 99) / 100
	if idx > 0 {
		idx--
	}
	return s[idx]
}
