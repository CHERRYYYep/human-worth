-- Agent Self-Claimed: independent single-work submissions, no cross-schema FK.
-- Initial identities, snapshots, positions and historical timestamps stay intact.
ALTER TABLE content.reviewed_entries DROP CONSTRAINT reviewed_entries_position_check;
ALTER TABLE content.reviewed_entries ADD CONSTRAINT reviewed_entries_position_check CHECK (position >= 0);
ALTER TABLE content.reviewed_entries ADD COLUMN origin text NOT NULL DEFAULT 'initial' CHECK (origin IN ('initial','additional'));
ALTER TABLE content.reviewed_entries ADD COLUMN author_id text;
ALTER TABLE content.reviewed_entries ADD COLUMN content jsonb;
ALTER TABLE content.reviewed_entries ADD CONSTRAINT reviewed_entries_projection_check CHECK (
 (origin='initial' AND author_id IS NULL AND content IS NULL) OR
 (origin='additional' AND author_id IS NOT NULL AND content IS NOT NULL AND jsonb_typeof(content)='object')
);
-- Reserve initial positions 0..31 even for new/old short bundles. Parent row
-- locks serialize allocation and takedown; never MAX()+1 outside that lock.
ALTER TABLE content.reviewed_tasks ADD COLUMN next_entry_position bigint NOT NULL DEFAULT 32 CHECK (next_entry_position BETWEEN 32 AND 2147483647);
ALTER TABLE content.reviewed_tasks ADD COLUMN catalog_revision bigint NOT NULL DEFAULT 1 CHECK (catalog_revision > 0);
CREATE TABLE content.entry_submissions (
 id text PRIMARY KEY,
 task_id text NOT NULL REFERENCES content.reviewed_tasks(task_id),
 author_id text NOT NULL,
 origin text NOT NULL DEFAULT 'additional' CHECK (origin='additional'),
 create_key text NOT NULL CHECK (length(create_key) BETWEEN 16 AND 128),
 create_hash bytea NOT NULL CHECK (octet_length(create_hash)=32),
 revision bigint NOT NULL CHECK (revision>0),
 state text NOT NULL CHECK (state IN ('pending_review','approved','rejected','approval_revoked')),
 content jsonb NOT NULL CHECK (jsonb_typeof(content)='object'),
 review_id text NOT NULL,
 rejection_reason text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 approved_at timestamptz,
 UNIQUE(author_id,task_id,create_key),
 CHECK ((state='rejected' AND rejection_reason IS NOT NULL AND length(btrim(rejection_reason))>0) OR (state<>'rejected' AND rejection_reason IS NULL))
);
CREATE INDEX entry_submissions_author_created_id ON content.entry_submissions(author_id,created_at DESC,id DESC);
CREATE TABLE content.entry_review_requests (
 id text PRIMARY KEY,
 entry_id text NOT NULL REFERENCES content.entry_submissions(id),
 submission_revision bigint NOT NULL CHECK (submission_revision>0),
 content_hash bytea NOT NULL CHECK (octet_length(content_hash)=32),
 snapshot jsonb NOT NULL,
 snapshot_hash text NOT NULL CHECK (snapshot_hash ~ '^[a-f0-9]{64}$'),
 state text NOT NULL CHECK (state IN ('pending_review','approved','rejected')),
 reason text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(entry_id,submission_revision),
 CHECK ((state='rejected' AND reason IS NOT NULL AND length(btrim(reason))>0) OR (state<>'rejected' AND reason IS NULL))
);
CREATE TABLE content.entry_review_outbox (
 id text PRIMARY KEY REFERENCES content.entry_review_requests(id),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts>=0),
 lease_token text,
 available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 delivered_at timestamptz,
 blocked boolean NOT NULL DEFAULT false
);
CREATE INDEX entry_review_outbox_pending ON content.entry_review_outbox(available_at,id) WHERE delivered_at IS NULL AND NOT blocked;
CREATE TABLE content.entry_decision_receipts (
 decision_id text PRIMARY KEY,
 review_id text NOT NULL UNIQUE REFERENCES content.entry_review_requests(id),
 request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
