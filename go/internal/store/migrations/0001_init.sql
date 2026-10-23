-- Review run state machine. See docs/architecture.md §3.

CREATE TABLE review_run (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  repo            text NOT NULL,
  pr_number       int  NOT NULL CHECK (pr_number > 0),
  head_sha        text NOT NULL,
  installation_id bigint NOT NULL,
  -- Keep in sync with go/internal/run/state.go.
  state           text NOT NULL CHECK (state IN (
                    'queued', 'running', 'previewing', 'reviewed', 'awaiting_approval',
                    'merging', 'done', 'failed', 'superseded')),
  attempt         int  NOT NULL DEFAULT 0,
  job_name        text,
  verdict         text CHECK (verdict IN ('approve', 'request_changes')),
  result          jsonb,
  -- Why the run failed; shown on the Check Run.
  error           text,
  budget_usd      numeric NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (repo, pr_number, head_sha),
  CHECK (state NOT IN ('reviewed', 'awaiting_approval', 'merging', 'done') OR verdict IS NOT NULL)
);

CREATE INDEX review_run_active ON review_run (state, created_at)
  WHERE state NOT IN ('done', 'failed', 'superseded');

-- Webhook deliveries already recorded; GitHub redelivers with the same ID.
CREATE TABLE webhook_delivery (
  delivery_id   text PRIMARY KEY,
  received_at   timestamptz NOT NULL DEFAULT now()
);

-- One row per GitHub side effect; external_id set means it already happened.
-- Keep kind in sync with go/internal/store/store.go.
CREATE TABLE github_action (
  run_id        uuid NOT NULL REFERENCES review_run(id),
  kind          text NOT NULL CHECK (kind IN (
                  'dismiss_stale_approvals', 'check_run_create', 'check_run_complete', 'review', 'merge')),
  external_id   text,
  PRIMARY KEY (run_id, kind)
);

-- Latest pi session snapshot, uploaded by the runner after each turn.
CREATE TABLE run_session (
  run_id        uuid PRIMARY KEY REFERENCES review_run(id),
  jsonl         bytea NOT NULL,
  turn          int NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE run_usage (
  run_id        uuid NOT NULL REFERENCES review_run(id),
  turn          int NOT NULL,
  model         text NOT NULL,
  input_tokens  int NOT NULL,
  output_tokens int NOT NULL,
  cost_usd      numeric NOT NULL,
  PRIMARY KEY (run_id, turn)
);
