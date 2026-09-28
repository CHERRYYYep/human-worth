-- Keyset pagination for an author's private drafts. Category remains a SQL
-- filter after the author range is selected so legacy long values stay migratable.
CREATE INDEX task_drafts_author_created_id
    ON content.task_drafts(author_id, created_at DESC, id DESC)
    WHERE state = 'draft';
