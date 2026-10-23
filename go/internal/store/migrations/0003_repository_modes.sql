-- Policy is immutable after the controller snapshots it, before starting a Job.
ALTER TABLE review_run ADD COLUMN repo_config jsonb;
ALTER TABLE review_run ADD COLUMN config_sha text;

-- A confirmation belongs only to the reviewed manual run current at receipt.
CREATE TABLE merge_signal (
 delivery_id text PRIMARY KEY REFERENCES webhook_delivery(delivery_id),
 run_id uuid NOT NULL REFERENCES review_run(id),
 actor text NOT NULL,
 review_id bigint NOT NULL DEFAULT 0,
 handled boolean NOT NULL DEFAULT false
);
CREATE INDEX merge_signal_pending ON merge_signal (run_id) WHERE NOT handled;
