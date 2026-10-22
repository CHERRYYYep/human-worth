-- Keep the manifest and confirmed offset in the same database transaction.
ALTER TABLE asset.assets
    ADD COLUMN storage_upload_id text NOT NULL DEFAULT '',
    ADD COLUMN storage_parts jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(storage_parts) = 'array'),
    ADD COLUMN storage_cleaned_at timestamptz;
CREATE INDEX assets_challenge_cleanup ON asset.assets(updated_at)
WHERE run_id <> '' AND state IN ('staging','failed') AND storage_cleaned_at IS NULL;
