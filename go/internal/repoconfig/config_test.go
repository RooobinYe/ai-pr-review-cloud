package repoconfig

import (
	"strings"
	"testing"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
)

func TestParseDefaults(t *testing.T) {
	for _, data := range []string{"", "# defaults", "{}"} {
		got, err := Parse([]byte(data), 0.5, "zh")
		if err != nil {
			t.Fatal(err)
		}
		if got.ApprovalMode != Vote || got.FailOn != review.SeverityHigh || got.BudgetUSD != 0.5 || got.Language != "zh" {
			t.Fatalf("defaults %+v", got)
		}
	}
}

func TestParseConfiguredPolicy(t *testing.T) {
	data := []byte("approvalMode: manual\nfailOn: medium\nbudgetUSD: 0.12\nlanguage: en\npreview:\n  enabled: true")
	got, err := Parse(data, 0.5, "zh")
	if err != nil {
		t.Fatal(err)
	}
	if got.ApprovalMode != Manual || got.FailOn != review.SeverityMedium || got.BudgetUSD != 0.12 || got.Language != "en" {
		t.Fatalf("configured policy %+v", got)
	}
	zero, err := Parse([]byte("budgetUSD: 0"), 0.5, "zh")
	if err != nil || zero.BudgetUSD != 0 {
		t.Fatalf("zero budget %+v, err %v", zero, err)
	}
}

func TestParseRejectsInvalidPolicy(t *testing.T) {
	tests := []struct{ name, yaml string }{
		{"mode", "approvalMode: typo"},
		{"threshold", "failOn: none"},
		{"negative budget", "budgetUSD: -1"},
		{"nan", "budgetUSD: .nan"},
		{"infinite", "budgetUSD: .inf"},
		{"empty language", "language: ''"},
		{"unknown field", "approvalMod: auto"},
		{"duplicate field", "approvalMode: auto\napprovalMode: vote"},
		{"multiple documents", "approvalMode: auto\n---\napprovalMode: vote"},
		{"oversize", strings.Repeat(" ", MaxBytes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := Parse([]byte(tt.yaml), 0.5, "zh"); err == nil {
				t.Fatalf("accepted invalid policy: %+v", got)
			}
		})
	}
}

func TestApply(t *testing.T) {
	tests := []struct {
		name      string
		threshold review.Severity
		verdict   review.Verdict
		comments  []review.Comment
		want      review.Verdict
	}{
		{"no findings", review.SeverityHigh, review.VerdictApprove, nil, review.VerdictApprove},
		{"below threshold", review.SeverityHigh, review.VerdictApprove, []review.Comment{{Severity: review.SeverityMedium}}, review.VerdictApprove},
		{"at threshold", review.SeverityHigh, review.VerdictApprove, []review.Comment{{Severity: review.SeverityHigh}}, review.VerdictRequestChanges},
		{"above threshold", review.SeverityHigh, review.VerdictApprove, []review.Comment{{Severity: review.SeverityCritical}}, review.VerdictRequestChanges},
		{"critical threshold", review.SeverityCritical, review.VerdictApprove, []review.Comment{{Severity: review.SeverityHigh}}, review.VerdictApprove},
		{"medium threshold", review.SeverityMedium, review.VerdictApprove, []review.Comment{{Severity: review.SeverityMedium}}, review.VerdictRequestChanges},
		{"low threshold", review.SeverityLow, review.VerdictApprove, []review.Comment{{Severity: review.SeverityLow}}, review.VerdictRequestChanges},
		{"later finding", review.SeverityHigh, review.VerdictApprove, []review.Comment{{Severity: review.SeverityLow}, {Severity: review.SeverityCritical}}, review.VerdictRequestChanges},
		{"model rejection is retained", review.SeverityHigh, review.VerdictRequestChanges, nil, review.VerdictRequestChanges},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := review.Result{Verdict: tt.verdict, Body: "Review", Comments: tt.comments}
			got := (Config{FailOn: tt.threshold}).Apply(res)
			if got.Verdict != tt.want {
				t.Errorf("verdict %s, want %s", got.Verdict, tt.want)
			}
			if len(got.Comments) != len(tt.comments) {
				t.Fatal("policy removed review findings")
			}
		})
	}
}
