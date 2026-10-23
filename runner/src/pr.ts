// Pull request context shared by both modes. In cluster mode the fetch init
// container writes it to /workspace/pr.json (go/internal/fetch mirrors this
// shape); in local mode github.ts builds it.
import { Type, type Static } from "typebox";
import { Value } from "typebox/value";

export const ChangedFileSchema = Type.Object({
  path: Type.String({ minLength: 1 }),
  /** GitHub file status: added, modified, removed, renamed, copied, changed, unchanged. */
  status: Type.String(),
  additions: Type.Integer({ minimum: 0 }),
  deletions: Type.Integer({ minimum: 0 }),
  previousPath: Type.Optional(Type.String({ minLength: 1 })),
});

/** An inline comment the reviewer left on an earlier commit of the PR. */
export const PreviousCommentSchema = Type.Object({
  path: Type.String({ minLength: 1 }),
  /** Line in the current diff, or in the commit it was left on when outdated. */
  line: Type.Integer({ minimum: 0 }),
  body: Type.String(),
  /** Someone marked the thread resolved. */
  resolved: Type.Boolean(),
  /** The line it pointed at has changed since. */
  outdated: Type.Boolean(),
});

export const PRContextSchema = Type.Object({
  /** owner/name */
  repo: Type.String({ pattern: "^[\\w.-]+/[\\w.-]+$" }),
  number: Type.Integer({ minimum: 1 }),
  title: Type.String(),
  author: Type.String(),
  body: Type.String(),
  baseRef: Type.String(),
  headRef: Type.String(),
  baseSha: Type.String({ minLength: 1 }),
  headSha: Type.String({ minLength: 1 }),
  files: Type.Array(ChangedFileSchema),
  /** Unified diff of the whole PR, one `diff --git` section per file. */
  diff: Type.String(),
  /** The reviewer's own inline comments on earlier commits; cluster mode only. */
  previousComments: Type.Optional(Type.Array(PreviousCommentSchema)),
});

export type ChangedFile = Static<typeof ChangedFileSchema>;
export type PreviousComment = Static<typeof PreviousCommentSchema>;
export type PRContext = Static<typeof PRContextSchema>;

/** Validates pr.json as written by the fetch init container. */
export function parsePRContext(value: unknown): PRContext {
  if (!Value.Check(PRContextSchema, value)) {
    const errors = [...Value.Errors(PRContextSchema, value)].map((e) => `${e.instancePath || "/"} ${e.message}`);
    throw new Error(`invalid PR context: ${errors.slice(0, 5).join("; ")}`);
  }
  return value;
}
