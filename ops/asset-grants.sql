\set ON_ERROR_STOP on
BEGIN;
GRANT USAGE ON SCHEMA asset TO asset_runtime;
GRANT SELECT ON asset.schema_migrations, asset.assets, asset.upload_sessions TO asset_runtime;
GRANT INSERT ON asset.assets, asset.upload_sessions TO asset_runtime;
GRANT UPDATE(state, detected_media_type, received_bytes, actual_sha256, completed_at, updated_at) ON asset.assets TO asset_runtime;
GRANT UPDATE(state, received_bytes, updated_at, storage_cleaned_at) ON asset.upload_sessions TO asset_runtime;
COMMIT;
