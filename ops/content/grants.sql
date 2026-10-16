-- Run with content_owner (or administrator) AFTER `content migrate`.
\set ON_ERROR_STOP on
BEGIN;
GRANT USAGE ON SCHEMA content TO content_runtime;
GRANT SELECT ON content.schema_migrations, content.task_drafts TO content_runtime;
GRANT INSERT ON content.task_drafts TO content_runtime;
GRANT UPDATE(content, revision, updated_at) ON content.task_drafts TO content_runtime;
GRANT SELECT, INSERT ON content.reviewed_tasks, content.reviewed_entries, content.asset_review_receipts TO content_runtime;
GRANT UPDATE(state, revision, updated_at) ON content.reviewed_tasks TO content_runtime;
GRANT UPDATE(state) ON content.reviewed_entries TO content_runtime;
-- No snapshot/source/entry-identity updates or receipt rewrites.
-- No DELETE, DDL, author/state/key updates or cross-schema privileges.
COMMIT;
