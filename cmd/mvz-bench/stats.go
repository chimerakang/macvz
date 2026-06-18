package main

import (
	"math"
	"sort"
)

// Distribution summarizes a set of samples for reporting.
type Distribution struct {
	N    int     `json:"n"`
	Min  float64 `json:"min"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// summarize computes order statistics over samples (which it sorts in place).
func summarize(samples []float64) Distribution {
	d := Distribution{N: len(samples)}
	if len(samples) == 0 {
		return d
	}
	sort.Float64s(samples)
	var sum float64
	for _, v := range samples {
		sum += v
	}
	d.Min = samples[0]
	d.Max = samples[len(samples)-1]
	d.Mean = sum / float64(len(samples))
	d.P50 = percentile(samples, 50)
	d.P90 = percentile(samples, 90)
	d.P99 = percentile(samples, 99)
	return d
}

// percentile returns the p-th percentile of an already-sorted slice using the
// nearest-rank method.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

func toFloats(d []int64) []float64 {
	out := make([]float64, len(d))
	for i, v := range d {
		out[i] = float64(v)
	}
	return out
}
