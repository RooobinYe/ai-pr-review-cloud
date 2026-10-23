// Package repoconfig defines the repository policy snapshotted for a run.
package repoconfig

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"gopkg.in/yaml.v3"
)

const MaxBytes = 64 << 10
const (
	Vote   = "vote"
	Auto   = "auto"
	Manual = "manual"
)

type Config struct {
	ApprovalMode string          `yaml:"approvalMode" json:"approvalMode"`
	FailOn       review.Severity `yaml:"failOn" json:"failOn"`
	BudgetUSD    float64         `yaml:"budgetUSD" json:"budgetUSD"`
	Language     string          `yaml:"language" json:"language"`
	// Preview is reserved for M5; retaining it allows the documented config today.
	Preview yaml.Node `yaml:"preview,omitempty" json:"-"`
}

func Parse(data []byte, budget float64, language string) (Config, error) {
	if language == "" {
		language = "zh"
	}
	c := Config{ApprovalMode: Vote, FailOn: review.SeverityHigh, BudgetUSD: budget, Language: language}
	if len(data) > MaxBytes {
		return c, fmt.Errorf("repository config exceeds %d bytes", MaxBytes)
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil && err != io.EOF {
		return c, fmt.Errorf("repository config: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("repository config must contain one YAML document")
	}
	if !slices.Contains([]string{Vote, Auto, Manual}, c.ApprovalMode) {
		return c, fmt.Errorf("invalid approvalMode %q", c.ApprovalMode)
	}
	if !slices.Contains(review.Severities, c.FailOn) {
		return c, fmt.Errorf("invalid failOn %q", c.FailOn)
	}
	if math.IsNaN(c.BudgetUSD) || math.IsInf(c.BudgetUSD, 0) || c.BudgetUSD < 0 {
		return c, fmt.Errorf("budgetUSD must be finite and non-negative")
	}
	if strings.TrimSpace(c.Language) == "" || len(c.Language) > 100 {
		return c, fmt.Errorf("language must be 1 to 100 bytes")
	}
	return c, nil
}

// Apply never upgrades a model's rejection to an approval.
func (c Config) Apply(r review.Result) review.Result {
	threshold := slices.Index(review.Severities, c.FailOn)
	if threshold < 0 {
		threshold = slices.Index(review.Severities, review.SeverityHigh)
	}
	if r.Verdict == review.VerdictApprove {
		for _, comment := range r.Comments {
			rank := slices.Index(review.Severities, comment.Severity)
			if rank >= 0 && rank <= threshold {
				r.Verdict = review.VerdictRequestChanges
				r.Body += "\n\nAI Review policy: approval withheld because an inline finding meets failOn=" + string(review.Severities[threshold]) + "."
				break
			}
		}
	}
	return r
}
