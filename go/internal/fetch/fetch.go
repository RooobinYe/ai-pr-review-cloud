// Package fetch prepares a runner workspace: it checks out the PR head and
// writes pr.json, the PR context the runner reviews (runner/src/pr.ts).
package fetch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
)

// cloneDepth is how much head history the runner's git tool can see.
const cloneDepth = 20

// ChangedFile mirrors ChangedFile in runner/src/pr.ts.
type ChangedFile struct {
	Path         string `json:"path"`
	Status       string `json:"status"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	PreviousPath string `json:"previousPath,omitempty"`
}

// PRContext mirrors PRContext in runner/src/pr.ts.
type PRContext struct {
	Repo    string        `json:"repo"`
	Number  int           `json:"number"`
	Title   string        `json:"title"`
	Author  string        `json:"author"`
	Body    string        `json:"body"`
	BaseRef string        `json:"baseRef"`
	HeadRef string        `json:"headRef"`
	BaseSHA string        `json:"baseSha"`
	HeadSHA string        `json:"headSha"`
	Files   []ChangedFile `json:"files"`
	Diff    string        `json:"diff"`
}

// BuildContext assembles the PR context from GitHub's pull and files responses.
func BuildContext(repo string, pull githubapp.Pull, files []githubapp.PullFile) PRContext {
	changed := make([]ChangedFile, len(files))
	for i, f := range files {
		changed[i] = ChangedFile{Path: f.Filename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, PreviousPath: f.PreviousFilename}
	}
	author := pull.User.Login
	if author == "" {
		author = "unknown"
	}
	return PRContext{
		Repo:    repo,
		Number:  pull.Number,
		Title:   pull.Title,
		Author:  author,
		Body:    pull.Body,
		BaseRef: pull.Base.Ref,
		HeadRef: pull.Head.Ref,
		BaseSHA: pull.Base.SHA,
		HeadSHA: pull.Head.SHA,
		Files:   changed,
		Diff:    BuildDiff(files),
	}
}

// BuildDiff adds file headers to GitHub's per-file patches. It must produce
// the same text as buildDiff in runner/src/diff.ts; contract/ tests both.
func BuildDiff(files []githubapp.PullFile) string {
	sections := make([]string, len(files))
	for i, f := range files {
		from := f.Filename
		if f.PreviousFilename != "" {
			from = f.PreviousFilename
		}
		oldSide, newSide := "--- a/"+from, "+++ b/"+f.Filename
		if f.Status == "added" {
			oldSide = "--- /dev/null"
		}
		if f.Status == "removed" {
			newSide = "+++ /dev/null"
		}
		body := "(diff not shown: binary or too large; inspect the file directly)"
		if f.Patch != nil {
			body = *f.Patch
		}
		sections[i] = strings.Join([]string{"diff --git a/" + from + " b/" + f.Filename, oldSide, newSide, body}, "\n")
	}
	return strings.Join(sections, "\n")
}

// HeadMovedError means the PR has a newer head than the run was created for;
// a newer run reviews that one.
type HeadMovedError struct{ Want, Got string }

func (e *HeadMovedError) Error() string {
	return fmt.Sprintf("PR head moved from %s to %s; the newer commit gets its own run", e.Want, e.Got)
}

// Options describes one workspace to prepare.
type Options struct {
	Repo      string // owner/name
	PRNumber  int
	HeadSHA   string
	Workspace string
	// GitURL is the git host, e.g. https://github.com.
	GitURL string
	Token  string
}

// Run fetches the PR context, checks out the head into Workspace/repo, and
// writes Workspace/pr.json.
func Run(ctx context.Context, gh *githubapp.Client, opts Options) error {
	pull, err := gh.GetPull(ctx, opts.Repo, opts.PRNumber)
	if err != nil {
		return err
	}
	if pull.Head.SHA != opts.HeadSHA {
		return &HeadMovedError{Want: opts.HeadSHA, Got: pull.Head.SHA}
	}
	files, err := gh.ListPullFiles(ctx, opts.Repo, opts.PRNumber)
	if err != nil {
		return err
	}
	pr := BuildContext(opts.Repo, pull, files)

	dir := filepath.Join(opts.Workspace, "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := Clone(ctx, dir, strings.TrimSuffix(opts.GitURL, "/")+"/"+opts.Repo+".git", opts.Token, pr.HeadSHA, pr.BaseSHA); err != nil {
		return err
	}
	data, err := json.Marshal(pr)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(opts.Workspace, "pr.json"), data, 0o644)
}

// Clone checks out head in dir with recent history and the base commit
// available. Blobs are fetched eagerly: the runner has no credentials or
// network to fetch them lazily. The token is passed through git's environment
// config, so it appears neither in argv nor in .git/config.
func Clone(ctx context.Context, dir, remote, token, head, base string) error {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic)
	}
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env = dir, env
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	if err := git("init", "-q"); err != nil {
		return err
	}
	if err := git("remote", "add", "origin", remote); err != nil {
		return err
	}
	if err := git("fetch", "-q", fmt.Sprintf("--depth=%d", cloneDepth), "origin", head, base); err != nil {
		return err
	}
	return git("checkout", "-q", "--detach", head)
}
