-- Reverse migration 000003.
-- WARNING: This will fail if any documents have NULL content (external blobs).
-- Ensure all blobs are inlined before running this migration.

ALTER TABLE documents
    ALTER COLUMN content SET NOT NULL,
    DROP COLUMN IF EXISTS blob_key;
