package api

import (
	"sort"
	"sync"
)

// Stats collects per-execution latency numbers. Startup latency
// (create -> executable) is the project's headline metric.
type Stats struct {
	mu        sync.Mutex
	startupMS []int64
	totalMS   []int64
}

func (s *Stats) Record(startupMS, totalMS int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startupMS = append(s.startupMS, startupMS)
	s.totalMS = append(s.totalMS, totalMS)
}

type StatsSummary struct {
	Count      int   `json:"count"`
	StartupP50 int64 `json:"startupP50Ms"`
	StartupP99 int64 `json:"startupP99Ms"`
	TotalP50   int64 `json:"totalP50Ms"`
	TotalP99   int64 `json:"totalP99Ms"`
}

func (s *Stats) Summary() StatsSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return StatsSummary{
		Count:      len(s.startupMS),
		StartupP50: percentile(s.startupMS, 50),
		StartupP99: percentile(s.startupMS, 99),
		TotalP50:   percentile(s.totalMS, 50),
		TotalP99:   percentile(s.totalMS, 99),
	}
}

func percentile(vals []int64, p int) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted)*p + 99) / 100 // ceil
	if idx > 0 {
		idx--
	}
	return sorted[idx]
}
