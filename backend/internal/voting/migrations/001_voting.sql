CREATE TABLE voting.catalog_gates (
 task_id text PRIMARY KEY,
 generation bigint NOT NULL CHECK(generation>=0),
 state text NOT NULL CHECK(state IN ('frozen','open','closed','unpublished')),
 change_id text NOT NULL,
 sha256 text NOT NULL DEFAULT '',
 snapshot bytea,
 tombstone boolean NOT NULL DEFAULT false
);
CREATE TABLE voting.catalog_changes (
 change_id text PRIMARY KEY,
 task_id text NOT NULL REFERENCES voting.catalog_gates(task_id),
 generation bigint NOT NULL CHECK(generation>0),
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 phase text NOT NULL CHECK(phase IN ('prepared','activated')),
 sha256 text NOT NULL DEFAULT '',
 UNIQUE(task_id,generation)
);
CREATE TABLE voting.account_task_guards (
 account_id text NOT NULL, task_id text NOT NULL REFERENCES voting.catalog_gates(task_id),
 lock_marker boolean NOT NULL DEFAULT false,
 PRIMARY KEY(account_id,task_id)
);
-- Permanent fact. runtime receives NO UPDATE/DELETE or ownership on this table.
CREATE TABLE voting.statistics_access (
 account_id text NOT NULL, task_id text NOT NULL REFERENCES voting.catalog_gates(task_id),
 closed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(account_id,task_id)
);
CREATE TABLE voting.ballots (
 id text PRIMARY KEY, account_id text NOT NULL, task_id text NOT NULL REFERENCES voting.catalog_gates(task_id),
 generation bigint NOT NULL, sha256 text NOT NULL,
 expires_at timestamptz NOT NULL DEFAULT (clock_timestamp()+interval '15 minutes')
);
CREATE INDEX ballots_expiry ON voting.ballots(expires_at,id);
CREATE TABLE voting.current_votes (
 account_id text NOT NULL, task_id text NOT NULL REFERENCES voting.catalog_gates(task_id),
 id text NOT NULL UNIQUE, entry_id text NOT NULL, revision bigint NOT NULL CHECK(revision>0),
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(account_id,task_id)
);
CREATE INDEX votes_task_entry ON voting.current_votes(task_id,entry_id);
