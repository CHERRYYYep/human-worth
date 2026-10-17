CREATE TABLE moderation.reviews (
 id text PRIMARY KEY,
 task_id text NOT NULL,
 submission_revision bigint NOT NULL CHECK(submission_revision>0),
 snapshot jsonb NOT NULL,
 snapshot_hash bytea NOT NULL CHECK(octet_length(snapshot_hash)=32),
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','rejected')),
 decision text CHECK(decision IN ('approve','reject')),
 decision_id text UNIQUE,
 reason text,
 reviewer_id text,
 created_at timestamptz NOT NULL,
 decided_at timestamptz,
 delivery_state text NOT NULL DEFAULT 'undecided' CHECK(delivery_state IN ('undecided','pending','applied','blocked')),
 receipt jsonb,
 UNIQUE(task_id,submission_revision),
 CHECK ((decision IS NULL AND decision_id IS NULL AND reviewer_id IS NULL AND decided_at IS NULL AND delivery_state='undecided')
 OR (decision IS NOT NULL AND decision_id IS NOT NULL AND reviewer_id IS NOT NULL AND decided_at IS NOT NULL AND delivery_state<>'undecided')),
 CHECK ((decision='reject' AND reason IS NOT NULL AND length(btrim(reason))>0) OR (decision IS DISTINCT FROM 'reject' AND reason IS NULL)),
 CHECK ((delivery_state='applied' AND receipt IS NOT NULL AND status<>'pending') OR (delivery_state<>'applied' AND receipt IS NULL AND status='pending'))
);
CREATE INDEX reviews_status_created_id ON moderation.reviews(status,created_at DESC,id DESC);
CREATE TABLE moderation.decision_outbox (
 id text PRIMARY KEY REFERENCES moderation.reviews(id),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 lease_token text,
 available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 delivered_at timestamptz,
 blocked boolean NOT NULL DEFAULT false
);
CREATE INDEX decision_outbox_pending ON moderation.decision_outbox(available_at,id) WHERE delivered_at IS NULL AND NOT blocked;
