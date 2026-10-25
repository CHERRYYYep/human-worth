-- Agent Self-Claimed: recoverable cross-service directory fence; own schema only.
CREATE TABLE content.voting_catalog_heads (
 task_id text PRIMARY KEY REFERENCES content.task_drafts(id),
 generation bigint NOT NULL DEFAULT 0 CHECK(generation>=0),
 active_change text
);
CREATE TABLE content.voting_catalog_changes (
 id text PRIMARY KEY,
 task_id text NOT NULL REFERENCES content.voting_catalog_heads(task_id),
 generation bigint NOT NULL CHECK(generation>0),
 method text NOT NULL CHECK(method IN ('task','entry','legacy','bootstrap')),
 request bytea NOT NULL,
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 stage text NOT NULL DEFAULT 'queued' CHECK(stage IN ('queued','applied','completed','blocked')),
 result bytea,
 catalog bytea,
 attempts integer NOT NULL DEFAULT 0,
 lease_token text,
 available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 completed_at timestamptz,
 UNIQUE(task_id,generation)
);
CREATE INDEX voting_catalog_pending ON content.voting_catalog_changes(available_at,id) WHERE stage IN ('queued','applied');
