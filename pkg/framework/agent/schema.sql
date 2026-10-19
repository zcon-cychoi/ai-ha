-- Agent enrollment. Applied at startup; creates the tables only when they are
-- absent and does not alter existing ones.

-- Tokens an operator issues to let one (or a few) agents enrol. They authorize
-- enrolment and nothing else: an agent cannot use one to receive work, and an
-- agent token cannot be used to enrol another agent.
CREATE TABLE IF NOT EXISTS registration_tokens (
    token_id    text        PRIMARY KEY,
    token_hash  bytea       NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    max_uses    integer     NOT NULL DEFAULT 1 CHECK (max_uses > 0),
    uses        integer     NOT NULL DEFAULT 0 CHECK (uses >= 0),
    note        text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz,
    CONSTRAINT registration_tokens_uses_within_limit CHECK (uses <= max_uses)
);

-- Enrolled agents. agent_id identifies an installation; system_id identifies
-- the machine whose backups it takes. They are separate so that reinstalling
-- the agent does not break the backup history, and so that a restored copy of
-- a system can be told apart from the original.
CREATE TABLE IF NOT EXISTS agents (
    agent_id              text        PRIMARY KEY,
    system_id             text        NOT NULL,
    token_hash            bytea       NOT NULL UNIQUE,
    hostname              text,
    agent_version         text,
    -- The framework sets this, never the client: an agent that booted from a
    -- restored disk must not be able to declare itself a normal one.
    mode                  text        NOT NULL
        CHECK (mode IN ('normal', 'recovery_registration')),
    registration_token_id text,
    -- The client's key for one enrolment attempt. A retry carries the same key
    -- and is refused as a duplicate rather than enrolling a second agent.
    registration_key      text,
    registered_at         timestamptz NOT NULL DEFAULT now(),
    last_seen_at          timestamptz,
    revoked_at            timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS agents_registration_key
    ON agents (registration_token_id, registration_key)
    WHERE registration_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS agents_system_id ON agents (system_id);
