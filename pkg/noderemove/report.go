package noderemove

import (
	"fmt"
	"strings"
)

// Render returns a human-readable, multi-line summary of a removal run, one line
// per step plus a final verdict. It is what `macvz-kubelet remove` prints.
func (r Result) Render() string {
	var b strings.Builder
	mode := ""
	if r.DryRun {
		mode = " (dry-run)"
	}
	fmt.Fprintf(&b, "Node removal: %s%s\n", r.Node, mode)
	for _, s := range r.Steps {
		line := fmt.Sprintf("  [%s] %s", s.Status, s.Name)
		if s.Detail != "" {
			line += " — " + s.Detail
		}
		if s.Err != nil {
			line += fmt.Sprintf(" (%v)", s.Err)
		}
		b.WriteString(line + "\n")
	}
	if r.DryRun {
		b.WriteString("Verdict: DRY-RUN (no changes made)\n")
	} else if r.OK() {
		b.WriteString("Verdict: REMOVED (no failed steps)\n")
	} else {
		b.WriteString("Verdict: PARTIAL — re-run after resolving the failed step(s) above\n")
	}
	return b.String()
}
