-- Durable completion marker for idempotent shared-storage cleanup.
ALTER TABLE asset.upload_sessions ADD COLUMN storage_cleaned_at timestamptz;
CREATE INDEX upload_sessions_pending_cleanup ON asset.upload_sessions(expires_at)
WHERE storage_cleaned_at IS NULL AND state IN ('aborted', 'expired');
