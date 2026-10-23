package controller

import (
	"fmt"
	"slices"
	"strings"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
)

// CheckName is the Check Run shown on every reviewed commit.
const CheckName = "AI Review"

// GitHub caps Check Run summaries at 65535 characters; review bodies are
// similar. Stay well below both.
const maxBodyBytes = 60000

// ReviewEvent is the vote for a verdict (docs/architecture.md §6.3): APPROVE
// counts toward required approvals; problems are reported with COMMENT, which
// never blocks merging the way REQUEST_CHANGES would.
func ReviewEvent(v review.Verdict) string {
	if v == review.VerdictApprove {
		return "APPROVE"
	}
	return "COMMENT"
}

func footer(res review.Result) string {
	return fmt.Sprintf("\n\n---\n<sub>AI Review · `%s` · %s</sub>", res.Model, res.PR.HeadSHA[:7])
}

func commentBody(c review.Comment) string {
	return fmt.Sprintf("**%s** · confidence %s\n\n%s", c.Severity, c.Confidence, strings.TrimSpace(c.Body))
}

// BuildReview turns a result into one PR review: the body plus inline comments,
// pinned to the reviewed commit so later pushes cannot shift the anchors.
func BuildReview(res review.Result, event string) githubapp.NewReview {
	comments := make([]githubapp.ReviewComment, len(res.Comments))
	for i, c := range res.Comments {
		comments[i] = githubapp.ReviewComment{Path: c.Path, Line: c.Line, Side: "RIGHT", Body: commentBody(c)}
	}
	return githubapp.NewReview{
		CommitID: res.PR.HeadSHA,
		Body:     truncate(strings.TrimSpace(res.Body), maxBodyBytes) + footer(res),
		Event:    event,
		Comments: comments,
	}
}

// BuildFoldedReview moves inline comments into the body. It is the fallback
// when GitHub rejects an inline anchor.
func BuildFoldedReview(res review.Result, event string) githubapp.NewReview {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(res.Body))
	if len(res.Comments) > 0 {
		fmt.Fprintf(&b, "\n\n### Comments (%d)\n", len(res.Comments))
		for _, c := range sortedComments(res.Comments) {
			fmt.Fprintf(&b, "\n#### `%s:%d`\n\n%s\n", c.Path, c.Line, commentBody(c))
		}
	}
	return githubapp.NewReview{CommitID: res.PR.HeadSHA, Body: truncate(b.String(), maxBodyBytes) + footer(res), Event: event}
}

func sortedComments(cs []review.Comment) []review.Comment {
	sorted := slices.Clone(cs)
	slices.SortStableFunc(sorted, func(a, b review.Comment) int {
		if d := slices.Index(review.Severities, a.Severity) - slices.Index(review.Severities, b.Severity); d != 0 {
			return d
		}
		if d := strings.Compare(a.Path, b.Path); d != 0 {
			return d
		}
		return a.Line - b.Line
	})
	return sorted
}

// InProgressCheckRun opens the Check Run for a run.
func InProgressCheckRun(runID, headSHA string) githubapp.CheckRun {
	return githubapp.CheckRun{
		Name:       CheckName,
		HeadSHA:    headSHA,
		ExternalID: runID,
		Status:     "in_progress",
		Output:     &githubapp.CheckRunOutput{Title: "Reviewing", Summary: "An AI reviewer is reading this commit."},
	}
}

// ReviewedCheckRun completes the Check Run to match the vote cast. Anything
// but an approval is neutral rather than failure: the vote, not the check,
// carries the verdict, and a failed check would block merging if the check
// were required.
func ReviewedCheckRun(res review.Result, event string) githubapp.CheckRun {
	conclusion, title := "success", "Approved"
	switch {
	case res.Verdict != review.VerdictApprove:
		conclusion, title = "neutral", "Changes suggested"
	case event != "APPROVE":
		conclusion, title = "neutral", "Approved an outdated commit; not counted"
	}
	switch n := len(res.Comments); n {
	case 0:
	case 1:
		title += " · 1 comment"
	default:
		title += fmt.Sprintf(" · %d comments", n)
	}
	return githubapp.CheckRun{
		Status:     "completed",
		Conclusion: conclusion,
		Output:     &githubapp.CheckRunOutput{Title: title, Summary: truncate(strings.TrimSpace(res.Body), maxBodyBytes) + footer(res)},
	}
}

// FailedCheckRun completes the Check Run of a run that produced no review.
func FailedCheckRun(reason string) githubapp.CheckRun {
	return githubapp.CheckRun{
		Status:     "completed",
		Conclusion: "failure",
		Output:     &githubapp.CheckRunOutput{Title: "Review failed", Summary: truncate(reason, maxBodyBytes)},
	}
}

// StaleApprovals returns the bot's approvals that are still in effect for
// commits other than head. They are dismissed before head is reviewed, since a
// later COMMENT vote would not replace them.
func StaleApprovals(reviews []githubapp.Review, botLogin, head string) []int64 {
	var ids []int64
	for _, r := range reviews {
		if r.User.Login == botLogin && r.State == "APPROVED" && r.CommitID != head {
			ids = append(ids, r.ID)
		}
	}
	return ids
}
