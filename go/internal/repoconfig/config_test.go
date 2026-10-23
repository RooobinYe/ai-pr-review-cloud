package repoconfig

import (
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tt := range []struct {
		name, yaml string
		bad        bool
	}{
		{"missing", "", false}, {"empty", "# defaults", false},
		{"configured", "approvalMode: manual\nfailOn: medium\nbudgetUSD: 0.12\nlanguage: en\npreview:\n  enabled: true", false},
		{"mode", "approvalMode: typo", true}, {"threshold", "failOn: none", true},
		{"negative budget", "budgetUSD: -1", true}, {"nan", "budgetUSD: .nan", true}, {"infinite", "budgetUSD: .inf", true},
		{"zero budget", "budgetUSD: 0", false}, {"empty language", "language: ''", true},
		{"unknown", "approvalMod: auto", true}, {"duplicate", "approvalMode: auto\napprovalMode: vote", true},
		{"multiple documents", "approvalMode: auto\n---\napprovalMode: vote", true},
		{"oversize", strings.Repeat(" ", MaxBytes+1), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(tt.yaml), 0.5, "zh")
			if (err != nil) != tt.bad {
				t.Fatalf("config %+v err %v", c, err)
			}
			if tt.name == "missing" && (c.ApprovalMode != Vote || c.BudgetUSD != 0.5 || c.Language != "zh" || c.FailOn != review.SeverityHigh) {
				t.Fatalf("defaults %+v", c)
			}
			if tt.name == "configured" && (c.ApprovalMode != Manual || c.BudgetUSD != 0.12 || c.Language != "en" || c.FailOn != review.SeverityMedium) {
				t.Fatalf("parsed %+v", c)
			}
		})
	}
}
func TestApply(t *testing.T) {
	for thresholdIndex, threshold := range review.Severities {
		for severityIndex, severity := range review.Severities {
			for _, v := range review.Verdicts {
				r := review.Result{Verdict: v, Body: "Review", Comments: []review.Comment{{Severity: severity}}}
				got := (Config{FailOn: threshold}).Apply(r)
				want := v
				if severityIndex <= thresholdIndex {
					want = review.VerdictRequestChanges
				}
				if got.Verdict != want {
					t.Errorf("threshold %s severity %s verdict %s: got %s", threshold, severity, v, got.Verdict)
				}
			}
		}
	}
}
