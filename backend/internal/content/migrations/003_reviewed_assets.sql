-- Content alone owns the reviewed snapshot and current display eligibility.
-- This is NOT an Asset ACL or a second public/private switch. No cross-schema FK.
CREATE TABLE content.reviewed_tasks (
    task_id text PRIMARY KEY REFERENCES content.task_drafts(id),
    source_revision bigint NOT NULL CHECK (source_revision > 0),
    revision bigint NOT NULL CHECK (revision > 0),
    state text NOT NULL CHECK (state IN ('published', 'taken_down')),
    snapshot jsonb NOT NULL CHECK (jsonb_typeof(snapshot) = 'object'
        AND snapshot ? 'entries' AND jsonb_typeof(snapshot->'entries') = 'array'),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE content.reviewed_entries (
    id text PRIMARY KEY,
    task_id text NOT NULL REFERENCES content.reviewed_tasks(task_id),
    position integer NOT NULL CHECK (position BETWEEN 0 AND 31),
    state text NOT NULL CHECK (state IN ('approved', 'approval_revoked')),
    UNIQUE (task_id, position)
);
CREATE TABLE content.asset_review_receipts (
    decision_id text PRIMARY KEY CHECK (length(decision_id) BETWEEN 16 AND 128),
    task_id text NOT NULL REFERENCES content.reviewed_tasks(task_id),
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    response jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
