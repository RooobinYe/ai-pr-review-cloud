-- Retries, timeouts and usage detail (M3). See docs/architecture.md §4.3.

-- When the current attempt was claimed; an attempt that outlives the runner
-- deadline fails even if its Job never started.
ALTER TABLE review_run ADD COLUMN claimed_at timestamptz;

-- Cache hits are billed differently from fresh input (§4.5).
ALTER TABLE run_usage
  ADD COLUMN cache_read_tokens  int NOT NULL DEFAULT 0,
  ADD COLUMN cache_write_tokens int NOT NULL DEFAULT 0;
