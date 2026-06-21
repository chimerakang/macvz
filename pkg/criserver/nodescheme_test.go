package criserver

import (
	"strings"
	"testing"
)

func TestNodeLabelsAndTaintFormat(t *testing.T) {
	labels := NodeLabels()
	if len(labels) != 2 {
		t.Fatalf("NodeLabels() = %v, want 2 entries", labels)
	}
	wantLabels := []string{
		NodeRuntimeLabel + "=" + NodeRuntimeLabelValue,
		NodeHostNamespaceLabel + "=" + NodeHostNamespaceUnsupported,
	}
	for i, want := range wantLabels {
		if labels[i] != want {
			t.Errorf("NodeLabels()[%d] = %q, want %q", i, labels[i], want)
		}
		if !strings.Contains(want, "=") {
			t.Errorf("label %q is not key=value", want)
		}
	}

	taint := NodeTaint()
	want := NodeHostNamespaceTaintKey + "=" + NodeHostNamespaceTaintValue + ":" + NodeHostNamespaceTaintEffect
	if taint != want {
		t.Errorf("NodeTaint() = %q, want %q", taint, want)
	}
	// NoSchedule, not NoExecute: a deliberately-tolerated Pod must not be evicted.
	if !strings.HasSuffix(taint, ":NoSchedule") {
		t.Errorf("taint effect must be NoSchedule, got %q", taint)
	}
}

func TestHostNamespaceSchedulingHintNamesScheme(t *testing.T) {
	hint := hostNamespaceSchedulingHint()
	for _, want := range []string{NodeTaint(), NodeHostNamespaceLabel, "Linux node", "nodeAffinity"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q does not mention %q", hint, want)
		}
	}
}
