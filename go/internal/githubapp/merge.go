package githubapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/repoconfig"
)

// RepositoryConfig reads from the trusted base commit, never the PR head.
func (c *Client) RepositoryConfig(ctx context.Context, repo, ref string) ([]byte, error) {
	var f struct {
		Type, Encoding, Content string
		Size                    int
	}
	var raw json.RawMessage
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/contents/.ai-pr-review.yaml?ref=%s", repo, url.QueryEscape(ref)), nil, &raw)
	if StatusOf(err) == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, &APIError{Status: http.StatusUnprocessableEntity, Message: "repository config must be a file"}
	}
	if f.Type != "file" || f.Encoding != "base64" || f.Size > repoconfig.MaxBytes {
		return nil, &APIError{Status: http.StatusUnprocessableEntity, Message: "repository config must be a file of at most 64 KiB"}
	}
	data, err := base64.StdEncoding.DecodeString(f.Content)
	if err != nil {
		return nil, &APIError{Status: http.StatusUnprocessableEntity, Message: "repository config has invalid base64"}
	}
	return data, nil
}

func (c *Client) CanWrite(ctx context.Context, repo, login string) (bool, error) {
	var out struct{ Permission string }
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/collaborators/%s/permission", repo, url.PathEscape(login)), nil, &out)
	if StatusOf(err) == http.StatusNotFound {
		return false, nil
	}
	return out.Permission == "admin" || out.Permission == "write" || out.Permission == "maintain", err
}

func (c *Client) GetReview(ctx context.Context, repo string, number int, id int64) (Review, error) {
	var out Review
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/reviews/%d", repo, number, id), nil, &out)
	return out, err
}

// MergePull binds the operation to the reviewed head. GitHub enforces branch protection.
func (c *Client) MergePull(ctx context.Context, repo string, number int, sha string) (string, error) {
	var out struct {
		SHA     string
		Merged  bool
		Message string
	}
	err := c.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, number), map[string]string{"sha": sha, "merge_method": "squash"}, &out)
	if err != nil {
		return "", err
	}
	if !out.Merged || out.SHA == "" {
		return "", &APIError{Status: http.StatusMethodNotAllowed, Message: out.Message}
	}
	return out.SHA, nil
}
