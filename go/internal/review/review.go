// Package review defines the review result the runner hands to the controller
// (schema v3, docs/architecture.md §4.4). runner/src/schema.ts defines the same
// schema; contract/ holds the generated JSON schema and examples both sides test.
package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
)

// SchemaVersion is bumped on breaking changes to Result.
const SchemaVersion = 3

// MaxComments caps inline comments per review.
const MaxComments = 15

// Verdict is the model's review decision. The controller votes APPROVE for
// approve and COMMENT for request_changes (docs/architecture.md §6.3). The
// store mirrors these values in the review_run.verdict CHECK constraint; keep
// both in sync.
type Verdict string

const (
	VerdictApprove        Verdict = "approve"
	VerdictRequestChanges Verdict = "request_changes"
)

// Severity of an inline comment, most severe first.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

// Confidence the model has in an inline comment.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

var (
	Verdicts    = []Verdict{VerdictApprove, VerdictRequestChanges}
	Severities  = []Severity{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow}
	Confidences = []Confidence{ConfidenceHigh, ConfidenceMedium, ConfidenceLow}
	repoPattern = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)
	shaPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// PR identifies the reviewed commit, so the controller can reject a result
// that belongs to a different run.
type PR struct {
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	HeadSHA string `json:"head_sha"`
}

// Comment is an inline review comment on the new side of the diff.
type Comment struct {
	Path       string     `json:"path"`
	Line       int        `json:"line"`
	Severity   Severity   `json:"severity"`
	Confidence Confidence `json:"confidence"`
	Body       string     `json:"body"`
}

// Result is the runner's output for one run.
type Result struct {
	SchemaVersion int       `json:"schema_version"`
	PR            PR        `json:"pr"`
	Model         string    `json:"model"`
	GeneratedAt   string    `json:"generated_at"`
	Verdict       Verdict   `json:"verdict"`
	Body          string    `json:"body"`
	Comments      []Comment `json:"comments"`
}

// Parse decodes and validates a result, rejecting unknown fields.
func Parse(data []byte) (Result, error) {
	var r Result
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Result{}, fmt.Errorf("decode review result: %w", err)
	}
	if err := r.Validate(); err != nil {
		return Result{}, err
	}
	return r, nil
}

// Validate checks the constraints of the JSON schema that the type system does not.
func (r Result) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	check(r.SchemaVersion == SchemaVersion, "schema_version %d, want %d", r.SchemaVersion, SchemaVersion)
	check(repoPattern.MatchString(r.PR.Repo), "pr.repo %q is not owner/name", r.PR.Repo)
	check(r.PR.Number >= 1, "pr.number must be positive")
	check(shaPattern.MatchString(r.PR.HeadSHA), "pr.head_sha %q is not a full commit SHA", r.PR.HeadSHA)
	check(r.Model != "", "model is required")
	_, err := time.Parse(time.RFC3339, r.GeneratedAt)
	check(err == nil, "generated_at %q is not an RFC 3339 timestamp", r.GeneratedAt)
	check(slices.Contains(Verdicts, r.Verdict), "unknown verdict %q", r.Verdict)
	check(r.Body != "", "body is required")
	check(r.Comments != nil, "comments is required")
	check(len(r.Comments) <= MaxComments, "%d comments, at most %d allowed", len(r.Comments), MaxComments)
	for i, c := range r.Comments {
		check(c.Path != "", "comments[%d].path is required", i)
		check(c.Line >= 1, "comments[%d].line must be positive", i)
		check(slices.Contains(Severities, c.Severity), "comments[%d]: unknown severity %q", i, c.Severity)
		check(slices.Contains(Confidences, c.Confidence), "comments[%d]: unknown confidence %q", i, c.Confidence)
		check(c.Body != "", "comments[%d].body is required", i)
	}
	return errors.Join(errs...)
}
