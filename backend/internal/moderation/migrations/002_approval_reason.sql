-- Agent Self-Claimed: preserve 001/checksum and upgrade existing review history.
DO $$
DECLARE reason_constraint text;
BEGIN
 SELECT conname INTO STRICT reason_constraint FROM pg_constraint
 WHERE conrelid='moderation.reviews'::regclass AND contype='c'
 AND pg_get_constraintdef(oid) LIKE '%btrim(reason)%';
 EXECUTE format('ALTER TABLE moderation.reviews DROP CONSTRAINT %I', reason_constraint);
END $$;
ALTER TABLE moderation.reviews ADD CONSTRAINT reviews_decision_reason_check CHECK (
 CASE
 WHEN decision IS NULL THEN reason IS NULL
 WHEN decision='approve' THEN reason IS NULL OR octet_length(reason)<=4000
 WHEN decision='reject' THEN reason IS NOT NULL AND length(btrim(reason))>0 AND octet_length(reason)<=4000
 ELSE false
 END
);
