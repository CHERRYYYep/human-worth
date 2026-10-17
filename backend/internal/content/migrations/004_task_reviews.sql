-- Immutable submitted snapshots; no FK to Identity/Asset/Moderation schemas.
ALTER TABLE content.task_drafts DROP CONSTRAINT task_drafts_state_check;
ALTER TABLE content.task_drafts ADD CONSTRAINT task_drafts_state_check
 CHECK (state IN ('draft','pending_review','rejected','published','taken_down'));
ALTER TABLE content.task_drafts ADD COLUMN review_id text;
ALTER TABLE content.task_drafts ADD COLUMN rejection_reason text;
CREATE TABLE content.review_requests (
 id text PRIMARY KEY,
 task_id text NOT NULL REFERENCES content.task_drafts(id),
 author_id text NOT NULL,
 submission_revision bigint NOT NULL CHECK (submission_revision > 0),
 snapshot jsonb NOT NULL,
 state text NOT NULL CHECK (state IN ('pending_review','published','rejected')),
 reason text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(task_id,submission_revision),
 CHECK ((state='rejected' AND reason IS NOT NULL AND length(btrim(reason)) > 0) OR (state<>'rejected' AND reason IS NULL))
);
CREATE TABLE content.review_outbox (
 id text PRIMARY KEY REFERENCES content.review_requests(id),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts>=0),
 lease_token text,
 available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 delivered_at timestamptz
);
CREATE INDEX review_outbox_pending ON content.review_outbox(available_at,id) WHERE delivered_at IS NULL;
CREATE TABLE content.review_decision_receipts (
 decision_id text PRIMARY KEY,
 review_id text NOT NULL UNIQUE REFERENCES content.review_requests(id),
 request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
