package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIURL is the GitHub.com REST API.
const DefaultAPIURL = "https://api.github.com"

// maxPages bounds paginated listings; 30 pages of 100 covers GitHub's 3000-file PR cap.
const maxPages = 30

// Client calls the GitHub REST API with a bearer token.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Token returns the bearer token for each request.
	Token func(ctx context.Context) (string, error)
}

// StaticToken returns a token source that always yields token.
func StaticToken(token string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return token, nil }
}

// APIError is a non-2xx response from GitHub.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// StatusOf returns the HTTP status of a GitHub API error, or 0 for other errors.
func StatusOf(err error) int {
	var e *APIError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	token, err := c.Token(ctx)
	if err != nil {
		return fmt.Errorf("GitHub %s %s: token: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("GitHub %s %s: read body: %w", method, path, err)
	}
	if res.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
			Errors  []any  `json:"errors"`
		}
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &e) == nil && e.Message != "" {
			msg = e.Message
			if len(e.Errors) > 0 {
				detail, _ := json.Marshal(e.Errors)
				msg += ": " + string(detail)
			}
		}
		if len(msg) > 500 {
			// Cut on a rune boundary: the message ends up in a PostgreSQL text column.
			msg = strings.ToValidUTF8(msg[:500], "")
		}
		return &APIError{Method: method, Path: path, Status: res.StatusCode, Message: msg}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("GitHub %s %s: decode: %w", method, path, err)
	}
	return nil
}

// list fetches every page of a listing endpoint (path must not carry a query).
func list[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var all []T
	for page := 1; page <= maxPages; page++ {
		var batch []T
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", path, page), nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return all, nil
}

// Pull is the subset of a pull request the service uses.
type Pull struct {
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Number         int    `json:"number"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	State          string `json:"state"`
	User           struct {
		Login string `json:"login"`
	} `json:"user"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

// PullFile is one changed file of a pull request.
type PullFile struct {
	Filename         string `json:"filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	PreviousFilename string `json:"previous_filename,omitempty"`
	// Patch is absent for binary files and very large diffs.
	Patch *string `json:"patch,omitempty"`
}

func (c *Client) GetPull(ctx context.Context, repo string, number int) (Pull, error) {
	var p Pull
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &p)
	return p, err
}

func (c *Client) ListPullFiles(ctx context.Context, repo string, number int) ([]PullFile, error) {
	return list[PullFile](ctx, c, fmt.Sprintf("/repos/%s/pulls/%d/files", repo, number))
}

// Review is a submitted pull request review.
type Review struct {
	ID   int64 `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	// State is APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, or PENDING.
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
	Body     string `json:"body"`
}

func (c *Client) ListReviews(ctx context.Context, repo string, number int) ([]Review, error) {
	return list[Review](ctx, c, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number))
}

func (c *Client) DismissReview(ctx context.Context, repo string, number int, reviewID int64, message string) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/reviews/%d/dismissals", repo, number, reviewID),
		map[string]string{"message": message, "event": "DISMISS"}, nil)
}

// ReviewComment is an inline comment on the new side of the diff.
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// NewReview is a pull request review to submit.
type NewReview struct {
	CommitID string `json:"commit_id"`
	Body     string `json:"body"`
	// Event is APPROVE, REQUEST_CHANGES, or COMMENT.
	Event    string          `json:"event"`
	Comments []ReviewComment `json:"comments,omitempty"`
}

// CreateReview submits a review and returns its ID.
func (c *Client) CreateReview(ctx context.Context, repo string, number int, r NewReview) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number), r, &out)
	return out.ID, err
}

// CheckRunOutput is the summary shown on a Check Run.
type CheckRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Text    string `json:"text,omitempty"`
}

// CheckRun creates or updates a Check Run. Empty fields are left unchanged on update.
type CheckRun struct {
	Name       string          `json:"name,omitempty"`
	HeadSHA    string          `json:"head_sha,omitempty"`
	ExternalID string          `json:"external_id,omitempty"`
	Status     string          `json:"status,omitempty"`
	Conclusion string          `json:"conclusion,omitempty"`
	Output     *CheckRunOutput `json:"output,omitempty"`
}

// CreateCheckRun creates a Check Run and returns its ID.
func (c *Client) CreateCheckRun(ctx context.Context, repo string, cr CheckRun) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/check-runs", repo), cr, &out)
	return out.ID, err
}

// FindCheckRun returns the ID of the named Check Run on sha whose external ID
// is externalID, or 0 if there is none.
func (c *Client) FindCheckRun(ctx context.Context, repo, sha, name, externalID string) (int64, error) {
	var out struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
			ExternalID string `json:"external_id"`
		} `json:"check_runs"`
	}
	path := fmt.Sprintf("/repos/%s/commits/%s/check-runs?check_name=%s&filter=all&per_page=100", repo, sha, url.QueryEscape(name))
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return 0, err
	}
	for _, cr := range out.CheckRuns {
		if cr.ExternalID == externalID {
			return cr.ID, nil
		}
	}
	return 0, nil
}

func (c *Client) UpdateCheckRun(ctx context.Context, repo string, id int64, cr CheckRun) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/check-runs/%d", repo, id), cr, nil)
}

// graphQLPath locates the GraphQL endpoint next to the REST API: /graphql on
// GitHub.com, /api/graphql on GitHub Enterprise Server (REST at /api/v3).
func (c *Client) graphQLPath() (base, path string) {
	base = strings.TrimSuffix(c.BaseURL, "/")
	if b, ok := strings.CutSuffix(base, "/api/v3"); ok {
		return b, "/api/graphql"
	}
	return base, "/graphql"
}

// graphQL runs a query and decodes its data into out.
func (c *Client) graphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	base, path := c.graphQLPath()
	gql := *c
	gql.BaseURL = base
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := gql.do(ctx, http.MethodPost, path, map[string]any{"query": query, "variables": vars}, &res); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("GitHub GraphQL: %s", res.Errors[0].Message)
	}
	return json.Unmarshal(res.Data, out)
}

// ReviewThread is an inline review thread, represented by its first comment.
type ReviewThread struct {
	Path string
	// Line is the thread's line in the current diff, or in the commit it was
	// left on when the line no longer exists.
	Line        int
	Resolved    bool
	Outdated    bool
	AuthorLogin string
	Body        string
}

const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(last: 100) {
        nodes {
          isResolved
          isOutdated
          path
          line
          originalLine
          comments(first: 1) { nodes { author { login } body } }
        }
      }
    }
  }
}`

// ListReviewThreads returns the PR's most recent 100 review threads. Whether a
// thread is resolved is only exposed through GraphQL.
func (c *Client) ListReviewThreads(ctx context.Context, repo string, number int) ([]ReviewThread, error) {
	owner, name, _ := strings.Cut(repo, "/")
	var data struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []struct {
						IsResolved   bool   `json:"isResolved"`
						IsOutdated   bool   `json:"isOutdated"`
						Path         string `json:"path"`
						Line         *int   `json:"line"`
						OriginalLine *int   `json:"originalLine"`
						Comments     struct {
							Nodes []struct {
								Author *struct {
									Login string `json:"login"`
								} `json:"author"`
								Body string `json:"body"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	err := c.graphQL(ctx, reviewThreadsQuery, map[string]any{"owner": owner, "name": name, "number": number}, &data)
	if err != nil {
		return nil, fmt.Errorf("list review threads of %s#%d: %w", repo, number, err)
	}
	var threads []ReviewThread
	for _, n := range data.Repository.PullRequest.ReviewThreads.Nodes {
		if len(n.Comments.Nodes) == 0 {
			continue
		}
		first := n.Comments.Nodes[0]
		t := ReviewThread{Path: n.Path, Resolved: n.IsResolved, Outdated: n.IsOutdated, Body: first.Body}
		if first.Author != nil {
			t.AuthorLogin = first.Author.Login
		}
		switch {
		case n.Line != nil:
			t.Line = *n.Line
		case n.OriginalLine != nil:
			t.Line = *n.OriginalLine
		}
		threads = append(threads, t)
	}
	return threads, nil
}
