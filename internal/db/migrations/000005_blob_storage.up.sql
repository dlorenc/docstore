-- Migration 000003: external blob storage support
-- content becomes nullable (NULL = external blob referenced by blob_key)
-- blob_key = the key used in the external blob store (SHA256 of content)

ALTER TABLE documents
    ALTER COLUMN content DROP NOT NULL,
    ADD COLUMN blob_key TEXT;
