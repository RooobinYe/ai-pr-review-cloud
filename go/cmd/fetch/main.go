// Command fetch runs as the runner Pod's init container. It holds the only
// GitHub token in the Pod: it clones the PR head, fetches the base, and writes
// pr.json to the shared workspace, then exits so the token goes with it.
package main

import (
	"fmt"
	"os"
)

func main() {
	for _, key := range []string{"GITHUB_TOKEN", "REPO", "PR_NUMBER", "HEAD_SHA", "WORKSPACE"} {
		if os.Getenv(key) == "" {
			fmt.Fprintf(os.Stderr, "fetch: %s is required\n", key)
			os.Exit(1)
		}
	}
	// M1: shallow-clone HEAD_SHA into $WORKSPACE/repo and write $WORKSPACE/pr.json.
	fmt.Fprintln(os.Stderr, "fetch: not implemented yet (milestone M1)")
	os.Exit(1)
}
