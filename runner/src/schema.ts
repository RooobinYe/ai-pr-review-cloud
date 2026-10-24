// Review result schema v4 (docs/architecture.md §4.4). The model decides the
// verdict and writes the review body itself; structure exists only where the
// controller needs it: the verdict drives the state machine, path/line anchor
// inline comments, and severity feeds the failOn gate.
//
// go/internal/review mirrors this schema. contract/review-result.schema.json is
// generated from ReviewResultSchema and checked by tests on both sides.
import { Type, type Static } from "typebox";

export const SCHEMA_VERSION = 4;

export const VERDICTS = ["approve", "request_changes"] as const;
export const SEVERITIES = ["critical", "high", "medium", "low"] as const;
export const MAX_COMMENTS = 15;

export const ReviewCommentSchema = Type.Object(
  {
    path: Type.String({ minLength: 1, description: "File path relative to the repository root, as shown in the diff." }),
    line: Type.Integer({ minimum: 1, description: "Line number in the new version of the file. Must be an added or context line of the diff." }),
    severity: Type.Enum(SEVERITIES),
    body: Type.String({
      minLength: 1,
      description: "Markdown comment: the evidence (quote the code), why it matters, and a concrete fix.",
    }),
  },
  { additionalProperties: false },
);

/** Parameters of the submit_review tool: everything the model decides. */
export const ReviewSubmissionSchema = Type.Object(
  {
    verdict: Type.Enum(VERDICTS, {
      description: "approve if the PR is safe to merge as is; request_changes if anything must be fixed first.",
    }),
    body: Type.String({
      minLength: 1,
      description: "Markdown review summary posted as the top-level PR review comment.",
    }),
    comments: Type.Array(ReviewCommentSchema, {
      maxItems: MAX_COMMENTS,
      description: "Inline comments anchored to lines of the diff. Empty when there is nothing specific to point at.",
    }),
  },
  { additionalProperties: false },
);

/** What the runner hands to the controller (cluster mode) or prints (local mode). */
export const ReviewResultSchema = Type.Object(
  {
    schema_version: Type.Literal(SCHEMA_VERSION),
    pr: Type.Object(
      {
        repo: Type.String({ pattern: "^[\\w.-]+/[\\w.-]+$" }),
        number: Type.Integer({ minimum: 1 }),
        head_sha: Type.String({ pattern: "^[0-9a-f]{40}$" }),
      },
      { additionalProperties: false },
    ),
    model: Type.String({ minLength: 1 }),
    generated_at: Type.String({ format: "date-time" }),
    verdict: ReviewSubmissionSchema.properties.verdict,
    body: ReviewSubmissionSchema.properties.body,
    comments: ReviewSubmissionSchema.properties.comments,
  },
  { additionalProperties: false },
);

export type ReviewComment = Static<typeof ReviewCommentSchema>;
export type ReviewSubmission = Static<typeof ReviewSubmissionSchema>;
export type ReviewResult = Static<typeof ReviewResultSchema>;
export type Verdict = ReviewSubmission["verdict"];
export type Severity = ReviewComment["severity"];
