CREATE TABLE asset.assets (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    run_id text NOT NULL DEFAULT '',
    attempt_id text NOT NULL DEFAULT '',
    upload_id text NOT NULL DEFAULT '',
    state text NOT NULL CHECK (state IN ('staging', 'ready', 'failed', 'deleted')),
    original_filename text NOT NULL,
    declared_media_type text NOT NULL,
    detected_media_type text,
    expected_bytes bigint NOT NULL CHECK (expected_bytes > 0 AND expected_bytes <= 33554432),
    received_bytes bigint NOT NULL DEFAULT 0 CHECK (received_bytes >= 0),
    expected_sha256 bytea NOT NULL CHECK (octet_length(expected_sha256) = 32),
    actual_sha256 bytea CHECK (actual_sha256 IS NULL OR octet_length(actual_sha256) = 32),
    staging_key text NOT NULL UNIQUE,
    final_key text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE asset.upload_sessions (
    id text PRIMARY KEY,
    asset_id text NOT NULL UNIQUE REFERENCES asset.assets(id),
    owner_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_fingerprint bytea NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    state text NOT NULL CHECK (state IN ('active', 'completed', 'aborted', 'expired')),
    received_bytes bigint NOT NULL DEFAULT 0 CHECK (received_bytes >= 0),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE UNIQUE INDEX upload_sessions_owner_key ON asset.upload_sessions(owner_id, idempotency_key);
CREATE INDEX assets_owner_created_id ON asset.assets(owner_id, created_at DESC, id DESC);
CREATE INDEX upload_sessions_expiry ON asset.upload_sessions(expires_at) WHERE state = 'active';

CREATE UNIQUE INDEX assets_challenge_upload ON asset.assets(run_id, attempt_id, upload_id) WHERE upload_id <> '';
