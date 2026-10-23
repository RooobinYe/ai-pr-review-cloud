// Command fetch runs as the runner Pod's init container. It holds the only
// GitHub token in the Pod: it clones the PR head, fetches the base, and writes
// pr.json to the shared workspace, then exits so the token goes with it.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/fetch"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
)

func main() {
	for _, key := range []string{"GITHUB_TOKEN", "REPO", "PR_NUMBER", "HEAD_SHA", "WORKSPACE"} {
		if os.Getenv(key) == "" {
			fail("%s is required", key)
		}
	}
	number, err := strconv.Atoi(os.Getenv("PR_NUMBER"))
	if err != nil {
		fail("PR_NUMBER: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	token := os.Getenv("GITHUB_TOKEN")
	gh := &githubapp.Client{BaseURL: envOr("GITHUB_API_URL", githubapp.DefaultAPIURL), Token: githubapp.StaticToken(token)}
	opts := fetch.Options{
		Repo:      os.Getenv("REPO"),
		PRNumber:  number,
		HeadSHA:   os.Getenv("HEAD_SHA"),
		Workspace: os.Getenv("WORKSPACE"),
		GitURL:    envOr("GITHUB_URL", "https://github.com"),
		Token:     token,
	}
	if err := fetch.Run(ctx, gh, opts); err != nil {
		// The message becomes the container's termination message, which the
		// controller reports on the Check Run.
		fail("%v", err)
	}
	fmt.Fprintf(os.Stderr, "fetch: checked out %s#%d at %s\n", opts.Repo, opts.PRNumber, opts.HeadSHA)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fetch: "+format+"\n", args...)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
