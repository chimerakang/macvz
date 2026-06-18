package main

import "testing"

func TestSummarize(t *testing.T) {
	d := summarize([]float64{5, 1, 3, 2, 4})
	if d.N != 5 {
		t.Errorf("N = %d, want 5", d.N)
	}
	if d.Min != 1 || d.Max != 5 {
		t.Errorf("min/max = %v/%v, want 1/5", d.Min, d.Max)
	}
	if d.Mean != 3 {
		t.Errorf("mean = %v, want 3", d.Mean)
	}
	if d.P50 != 3 {
		t.Errorf("p50 = %v, want 3", d.P50)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	if d := summarize(nil); d.N != 0 {
		t.Errorf("empty summarize N = %d, want 0", d.N)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	s := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if p := percentile(s, 90); p != 9 {
		t.Errorf("p90 = %v, want 9", p)
	}
	if p := percentile(s, 100); p != 10 {
		t.Errorf("p100 = %v, want 10", p)
	}
	if p := percentile(s, 1); p != 1 {
		t.Errorf("p1 = %v, want 1", p)
	}
}

func TestSplitVMStat(t *testing.T) {
	key, val, ok := splitVMStat("Pages active:                                 394162.")
	if !ok || key != "Pages active" || val != 394162 {
		t.Errorf("got (%q, %d, %v), want (Pages active, 394162, true)", key, val, ok)
	}
	if _, _, ok := splitVMStat("not a stat line"); ok {
		t.Error("expected non-stat line to fail parsing")
	}
}
