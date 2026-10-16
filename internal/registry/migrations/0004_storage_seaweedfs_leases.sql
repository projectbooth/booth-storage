-- SeaweedFS IAM leases (ADR 0118 ruling 3): the seaweedfs-iam credential-broker path
-- mints a bare access key/secret pair with no native expiry (unlike MinIO's expiring
-- service accounts), so this module tracks its own and a sweeper
-- (internal/seaweedfssweep) deletes what's expired. iam_user_name is a fresh, per-lease
-- username, never reused, so deleting one lease's user can never affect another's.
CREATE TABLE storage_seaweedfs_leases (
    id             UUID        NOT NULL,
    workspace      TEXT        NOT NULL,
    backend_id     TEXT        NOT NULL,
    iam_user_name  TEXT        NOT NULL,
    access_key_id  TEXT        NOT NULL,
    prefix         TEXT        NOT NULL,
    access         TEXT        NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    swept_at       TIMESTAMPTZ,          -- NULL until the sweeper has confirmed cleanup
    PRIMARY KEY (id)
);

-- The sweeper's own lookup: every lease past expiry that hasn't been confirmed cleaned
-- up yet, oldest first (the sweeper's own backlog-age metric needs the oldest one).
CREATE INDEX storage_seaweedfs_leases_sweep ON storage_seaweedfs_leases (expires_at) WHERE swept_at IS NULL;
