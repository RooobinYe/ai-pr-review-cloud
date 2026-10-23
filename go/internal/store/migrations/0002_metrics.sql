-- Phase timestamps and cache usage for the metrics of docs/architecture.md §7.1.

ALTER TABLE review_run
  -- First claimed by the controller; claimed_at tracks the current attempt.
  ADD COLUMN started_at        timestamptz,
  -- The fetch init container of the runner Pod, read from its status.
  ADD COLUMN fetch_started_at  timestamptz,
  ADD COLUMN fetch_finished_at timestamptz,
  -- The runner's result arrived.
  ADD COLUMN reviewed_at       timestamptz,
  -- Reached done, failed or superseded.
  ADD COLUMN finished_at       timestamptz;

ALTER TABLE run_usage
  ADD COLUMN IF NOT EXISTS cache_read_tokens  int NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS cache_write_tokens int NOT NULL DEFAULT 0;
