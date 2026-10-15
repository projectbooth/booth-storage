-- ADR 0117 point 8a: whether this backend has passed the one-time create-only
-- capability probe. Only meaningful for an S3 backend (registry.SupportsCreateOnly);
-- every other kind is reliable without probing.
ALTER TABLE storage_backends ADD COLUMN create_only_verified BOOLEAN NOT NULL DEFAULT FALSE;
