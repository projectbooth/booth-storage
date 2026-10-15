-- Upload grants (ADR 0117 "Reconciled design" item 1/8a): this module, not booth-core,
-- holds the record of which app may create-only-write into which backend prefix.
--
-- A grant is keyed by (workspace, principal, backend_id, prefix): a second grant for the
-- same principal and scope replaces the first outright (internal/uploadgrant.Service.Create),
-- mirroring booth-api's write-grants contract. id is a surrogate, not part of the key,
-- because one principal may hold distinct grants for different prefixes on the same backend.
CREATE TABLE storage_upload_grants (
    id          UUID        NOT NULL,
    workspace   TEXT        NOT NULL,
    principal   TEXT        NOT NULL,
    backend_id  TEXT        NOT NULL,
    prefix      TEXT        NOT NULL,
    extensions  TEXT[]      NOT NULL,
    max_bytes   BIGINT      NOT NULL,
    granted_by  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (id),
    UNIQUE (workspace, principal, backend_id, prefix)
);

-- The admission check's lookup: every live grant for one principal on one backend.
CREATE INDEX storage_upload_grants_admission ON storage_upload_grants (workspace, principal, backend_id);
