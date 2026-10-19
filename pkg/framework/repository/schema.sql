-- Backup repositories. Applied at startup; creates the table only when it is
-- absent.
CREATE TABLE IF NOT EXISTS repositories (
    repository_id          text        PRIMARY KEY,
    name                   text        NOT NULL UNIQUE,
    type                   text        NOT NULL CHECK (type IN ('s3')),
    provider               text        NOT NULL,
    -- The location below never changes after registration: backup sets
    -- already written here would otherwise be looked for somewhere else.
    endpoint               text        NOT NULL DEFAULT '',
    region                 text        NOT NULL DEFAULT '',
    bucket                 text        NOT NULL,
    prefix                 text        NOT NULL DEFAULT '',
    -- The access key, secret key and session token, sealed with the
    -- framework's secret key and bound to repository_id. Never plaintext.
    credentials            bytea       NOT NULL,
    has_session_token      boolean     NOT NULL DEFAULT false,
    created_at             timestamptz NOT NULL DEFAULT now(),
    credentials_updated_at timestamptz NOT NULL DEFAULT now()
);
