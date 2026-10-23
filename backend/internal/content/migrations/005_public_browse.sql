-- Unknown historical first-publication/approval times MUST remain NULL. Neither
-- mutable updated_at nor migration time proves those historical events.
ALTER TABLE content.reviewed_tasks ADD COLUMN published_at timestamptz;
ALTER TABLE content.reviewed_entries ADD COLUMN approved_at timestamptz;
-- Existing UNIQUE(task_id,position) supports the bounded initial-bundle keyset.
-- No duplicate index, backfill, author mutation or cross-schema privileges.
