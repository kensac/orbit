package engine

import "testing"

func TestFullReport(t *testing.T) {
	long := "## Findings\n\n1. One\n2. Two\n3. Three"
	for _, tc := range []struct {
		name, report, prose, want string
	}{
		{"a one-line pointer gives way to the prose", "See the chat reply for the full rundown.", long, long},
		{"an empty report gives way", "", "All good.", "All good."},
		{"a shorter report gives way", "Three findings.\n\nOne, two.", long, long},
		{"a fuller report stands", long + "\n4. Four", long, long + "\n4. Four"},
		{"no prose leaves the report", "Spend was fine.", "", "Spend was fine."},
		{"a short preamble does not replace a one-line report", "Spend was fine, $40 under budget.", "Let me check.", "Spend was fine, $40 under budget."},
	} {
		out := &Output{Summary: "s", Report: tc.report}
		used := fullReport(out, tc.prose)
		if out.Report != tc.want || out.Summary != "s" || (used != "") != (tc.want != tc.report) {
			t.Errorf("%s: report = %q, used %q", tc.name, out.Report, used)
		}
	}
}
