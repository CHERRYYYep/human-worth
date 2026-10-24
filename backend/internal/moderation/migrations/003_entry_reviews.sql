-- Keep task snapshots/hashes/receipts unchanged. Old task-only writers omit
-- object_kind; its default and the generated identity preserve their uniqueness.
ALTER TABLE moderation.reviews
 ADD COLUMN object_kind text NOT NULL DEFAULT 'task' CHECK (object_kind IN ('task','entry')),
 ADD COLUMN object_id text GENERATED ALWAYS AS
  (CASE WHEN object_kind='task' THEN task_id ELSE snapshot->'submission'->>'id' END) STORED NOT NULL;
ALTER TABLE moderation.reviews DROP CONSTRAINT reviews_task_id_submission_revision_key;
ALTER TABLE moderation.reviews ADD CONSTRAINT reviews_object_revision_key
 UNIQUE(object_kind,object_id,submission_revision);
ALTER TABLE moderation.reviews ADD CONSTRAINT reviews_entry_identity_check CHECK (
 object_kind='task' OR (
  object_id ~ '^ent_[a-f0-9]{32}$'
  AND (snapshot->'submission'->>'taskId') IS NOT DISTINCT FROM task_id
  AND (snapshot->'submission'->>'origin') IS NOT DISTINCT FROM 'additional'
  AND (snapshot->>'reviewId') IS NOT DISTINCT FROM id
  AND (snapshot->'submission'->>'reviewId') IS NOT DISTINCT FROM id
  AND (snapshot->'submission'->>'revision') IS NOT DISTINCT FROM submission_revision::text
 ));
