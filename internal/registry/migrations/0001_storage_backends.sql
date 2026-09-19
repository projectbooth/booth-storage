-- One row per registered storage backend (ADR 0035): a collection keyed by
-- (workspace, id), with no notion of a "current" backend.
--
-- Deliberately holds no secrets. Credentials live in Kubernetes Secrets (ADR 0020);
-- has_credentials only records that one was written, so the admin view can say
-- "credentials set" without reading the Secret.
CREATE TABLE storage_backends (
    workspace       TEXT        NOT NULL,
    id              TEXT        NOT NULL,
    display_name    TEXT        NOT NULL,
    kind            TEXT        NOT NULL CHECK (kind IN ('s3', 'filesystem', 'azure', 'gcs')),
    config          JSONB       NOT NULL,
    has_credentials BOOLEAN     NOT NULL DEFAULT FALSE,
    created_by      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace, id)
);
