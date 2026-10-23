// Package githubapp holds the GitHub App integration: webhook verification and
// translating webhook events into review triggers.
package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// VerifySignature reports whether sig is a valid X-Hub-Signature-256 header
// ("sha256=<hex>") for body under secret. An empty secret never verifies.
func VerifySignature(secret, body []byte, sig string) bool {
	if len(secret) == 0 {
		return false
	}
	hexSig, ok := strings.CutPrefix(sig, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Trigger identifies one PR revision that should be reviewed.
type Trigger struct {
	Repo           string // owner/name
	PRNumber       int
	HeadSHA        string
	InstallationID int64
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// reviewActions are the pull_request actions that produce a new revision to review.
var reviewActions = map[string]bool{"opened": true, "synchronize": true, "reopened": true}

type pullRequestEvent struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// TriggerFromEvent maps a webhook delivery to a review trigger. It returns
// ok=false for events and actions that do not start a review.
func TriggerFromEvent(eventType string, body []byte) (t Trigger, ok bool, err error) {
	if eventType != "pull_request" {
		return Trigger{}, false, nil
	}
	var ev pullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return Trigger{}, false, fmt.Errorf("decode pull_request event: %w", err)
	}
	if !reviewActions[ev.Action] {
		return Trigger{}, false, nil
	}
	t = Trigger{
		Repo:           ev.Repository.FullName,
		PRNumber:       ev.PullRequest.Number,
		HeadSHA:        ev.PullRequest.Head.SHA,
		InstallationID: ev.Installation.ID,
	}
	if t.Repo == "" || t.PRNumber <= 0 || t.HeadSHA == "" || t.InstallationID == 0 {
		return Trigger{}, false, fmt.Errorf("pull_request event missing repository, number, head sha, or installation")
	}
	if !shaPattern.MatchString(t.HeadSHA) {
		return Trigger{}, false, fmt.Errorf("pull_request event head sha %q is not a full commit SHA", t.HeadSHA)
	}
	return t, true, nil
}

// ApprovalEvent is a human confirmation, not a request to create another run.
type ApprovalEvent struct {
	Repo           string
	PRNumber       int
	InstallationID int64
	HeadSHA        string
	Actor          string
	ReviewID       int64
	CreatedAt      time.Time
}

func ApprovalFromEvent(kind string, body []byte) (ApprovalEvent, bool, error) {
	var a ApprovalEvent
	if kind != "issue_comment" && kind != "pull_request_review" {
		return a, false, nil
	}
	type user struct {
		Login string
		Type  string
	}
	var e struct {
		Action     string
		Repository struct {
			FullName string `json:"full_name"`
		}
		Installation struct{ ID int64 }
		Issue        struct {
			Number      int
			PullRequest *struct{} `json:"pull_request"`
		}
		Comment struct {
			Body      string
			User      user
			CreatedAt time.Time `json:"created_at"`
		}
		PullRequest struct{ Number int } `json:"pull_request"`
		Review      struct {
			ID          int64
			State       string
			CommitID    string `json:"commit_id"`
			User        user
			SubmittedAt time.Time `json:"submitted_at"`
		}
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return a, false, fmt.Errorf("decode approval event: %w", err)
	}
	var actor user
	switch kind {
	case "issue_comment":
		if e.Action != "created" || e.Issue.PullRequest == nil || strings.TrimSpace(e.Comment.Body) != "/merge" {
			return a, false, nil
		}
		a.PRNumber, a.CreatedAt, actor = e.Issue.Number, e.Comment.CreatedAt, e.Comment.User
	case "pull_request_review":
		if e.Action != "submitted" || !strings.EqualFold(e.Review.State, "approved") {
			return a, false, nil
		}
		a.PRNumber, a.HeadSHA, a.ReviewID, a.CreatedAt, actor = e.PullRequest.Number, e.Review.CommitID, e.Review.ID, e.Review.SubmittedAt, e.Review.User
		if !shaPattern.MatchString(a.HeadSHA) || a.ReviewID <= 0 {
			return a, false, fmt.Errorf("approval missing review id or commit")
		}
	}
	if actor.Type != "User" || strings.HasSuffix(actor.Login, "[bot]") {
		return a, false, nil
	}
	a.Repo, a.InstallationID, a.Actor = e.Repository.FullName, e.Installation.ID, actor.Login
	if a.Repo == "" || a.PRNumber <= 0 || a.InstallationID <= 0 || a.Actor == "" || a.CreatedAt.IsZero() {
		return a, false, fmt.Errorf("approval missing repository, PR, installation, actor or timestamp")
	}
	return a, true, nil
}
