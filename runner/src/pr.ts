// Pull request context shared by both modes. In cluster mode the fetch init
// container writes it to /workspace/pr.json; in local mode github.ts builds it.

export interface ChangedFile {
  path: string;
  /** GitHub file status: added, modified, removed, renamed, copied, changed, unchanged. */
  status: string;
  additions: number;
  deletions: number;
  previousPath?: string;
}

export interface PRContext {
  /** owner/name */
  repo: string;
  number: number;
  title: string;
  author: string;
  body: string;
  baseRef: string;
  headRef: string;
  baseSha: string;
  headSha: string;
  files: ChangedFile[];
  /** Unified diff of the whole PR, one `diff --git` section per file. */
  diff: string;
}
