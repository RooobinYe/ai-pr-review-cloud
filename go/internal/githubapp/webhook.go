// Package githubapp holds the GitHub App integration: webhook verification and
// translating webhook events into review triggers.
package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
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
	return t, true, nil
}
