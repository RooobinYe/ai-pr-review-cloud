package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
)

// contractDir is shared with runner/src/pr.test.ts.
const contractDir = "../../../contract/pr-context"

func loadContract(t *testing.T) ([]githubapp.PullFile, PRContext) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(contractDir, "github-files.json"))
	if err != nil {
		t.Fatal(err)
	}
	var files []githubapp.PullFile
	if err := json.Unmarshal(data, &files); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(contractDir, "pr.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pr PRContext
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pr); err != nil {
		t.Fatal(err)
	}
	return files, pr
}

func pullOf(pr PRContext) githubapp.Pull {
	var p githubapp.Pull
	p.Number, p.Title, p.Body, p.User.Login = pr.Number, pr.Title, pr.Body, pr.Author
	p.Base.Ref, p.Base.SHA, p.Head.Ref, p.Head.SHA = pr.BaseRef, pr.BaseSHA, pr.HeadRef, pr.HeadSHA
	return p
}

func TestBuildContextMatchesContract(t *testing.T) {
	files, want := loadContract(t)
	got := BuildContext(want.Repo, pullOf(want), files)
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("BuildContext differs from contract/pr-context/pr.json:\n%s", gj)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// upstream creates a repository at <root>/o/r.git with a base and a head commit.
func upstream(t *testing.T, root string) (base, head string) {
	t.Helper()
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "base")
	base = git(t, work, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "commit", "-q", "-am", "head")
	head = git(t, work, "rev-parse", "HEAD")
	bare := filepath.Join(root, "o", "r.git")
	git(t, work, "clone", "-q", "--bare", work, bare)
	git(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	return base, head
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	base, head := upstream(t, root)
	patch := "@@ -1 +1 @@\n-one\n+two"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ro-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/o/r/pulls/3":
			fmt.Fprintf(w, `{"number":3,"title":"t","user":{"login":"me"},"base":{"ref":"main","sha":%q},"head":{"ref":"f","sha":%q}}`, base, head)
		case "/repos/o/r/pulls/3/files":
			_ = json.NewEncoder(w).Encode([]githubapp.PullFile{{Filename: "a.txt", Status: "modified", Additions: 1, Deletions: 1, Patch: &patch}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	gh := &githubapp.Client{BaseURL: srv.URL, Token: githubapp.StaticToken("ro-token")}
	ws := t.TempDir()
	opts := Options{Repo: "o/r", PRNumber: 3, HeadSHA: head, Workspace: ws, GitURL: "file://" + root, Token: "ro-token"}
	if err := Run(context.Background(), gh, opts); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(ws, "pr.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pr PRContext
	if err := json.Unmarshal(data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.HeadSHA != head || pr.Author != "me" || !strings.Contains(pr.Diff, "+++ b/a.txt\n@@ -1 +1 @@") {
		t.Errorf("pr.json = %+v", pr)
	}
	repo := filepath.Join(ws, "repo")
	if got := git(t, repo, "rev-parse", "HEAD"); got != head {
		t.Errorf("checked out %s, want %s", got, head)
	}
	if out := git(t, repo, "diff", "--stat", base, head); !strings.Contains(out, "a.txt") {
		t.Errorf("base not available for git diff: %q", out)
	}
	if cfg, _ := os.ReadFile(filepath.Join(repo, ".git", "config")); strings.Contains(string(cfg), "ro-token") || strings.Contains(string(cfg), "extraHeader") {
		t.Errorf("credential persisted in .git/config:\n%s", cfg)
	}

	opts.HeadSHA, opts.Workspace = base, t.TempDir()
	var moved *HeadMovedError
	if err := Run(context.Background(), gh, opts); !errors.As(err, &moved) || moved.Got != head {
		t.Errorf("stale head: err %v, want HeadMovedError", err)
	}
}
