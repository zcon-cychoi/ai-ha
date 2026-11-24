-- Job queue. Applied by every command that opens the database, so each
-- statement is safe to repeat: tables are created only when absent, and the
-- changes made to existing tables (jobs_kind_check and the indexes below) are
-- made only when they are missing.
--
-- "Only when missing" is decided from the catalog, not left to IF NOT EXISTS.
-- CREATE INDEX IF NOT EXISTS locks its table before it finds the index there,
-- and this file runs as one transaction, so a command starting up held a lock
-- on jobs while it waited for one on attempts — the order a claim takes them
-- in reverse, which PostgreSQL ends as a deadlock.

-- A job is the logical work a user or an HA flow asked for. It outlives the
-- attempts made to carry it out, which is why the attempt is a separate row:
-- reporting is tied to an attempt, so a late report from a superseded one
-- cannot move the job.
CREATE TABLE IF NOT EXISTS jobs (
    job_id             text        PRIMARY KEY,
    system_id          text        NOT NULL,
    -- Constrained by jobs_kind_check below.
    kind               text        NOT NULL,
    status             text        NOT NULL
        CHECK (status IN ('queued', 'claimed', 'running', 'completed',
                          'failed', 'cancelling', 'cancelled')),
    -- What to do, passed through to the agent. Held as JSON so the shape can
    -- grow without a migration.
    spec               jsonb       NOT NULL,
    -- current_attempt_id is the attempt whose reports count. A report quoting
    -- any other attempt is ignored rather than applied.
    current_attempt_id text,
    -- cancel_requested is delivered to the agent in its next progress
    -- response: there is no inbound path to an agent.
    cancel_requested   boolean     NOT NULL DEFAULT false,
    message            text,
    requested_by       text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    started_at         timestamptz,
    finished_at        timestamptz
);

-- The kinds a job may have, matching the Kind constants. Tables created when
-- only backups existed carry an inline check under the same name that admits
-- 'backup' alone; it is replaced here. Replacing it locks the table and
-- re-reads every row, so it is done only when the constraint is missing or
-- does not yet name every kind. The constraint is written as an IN list so
-- that PostgreSQL shows each kind as its own quoted literal, which is what
-- the test for "names every kind" looks for.
DO $$
DECLARE
    kinds text[] := ARRAY['backup', 'replication-setup', 'replication-teardown',
                          'ha-demote', 'ha-promote', 'ha-rejoin'];
    def   text;
BEGIN
    SELECT pg_get_constraintdef(oid) INTO def
      FROM pg_constraint
     WHERE conrelid = 'jobs'::regclass AND conname = 'jobs_kind_check';
    IF def IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM unnest(kinds) AS k WHERE strpos(def, quote_literal(k)) = 0) THEN
        RETURN;
    END IF;
    ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_kind_check;
    EXECUTE format('ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check CHECK (kind IN (%s))',
                   (SELECT string_agg(quote_literal(k), ', ') FROM unnest(kinds) AS k));
END
$$;

-- A job waiting for an agent is looked up by system on every long poll, and
-- the sweeper walks jobs that can still time out.
DO $$
BEGIN
    IF to_regclass('jobs_queued') IS NULL THEN
        CREATE INDEX jobs_queued ON jobs (system_id, created_at) WHERE status = 'queued';
    END IF;
    IF to_regclass('jobs_unfinished') IS NULL THEN
        CREATE INDEX jobs_unfinished
            ON jobs (status) WHERE status IN ('queued', 'claimed', 'running', 'cancelling');
    END IF;
END
$$;

-- One execution of a job by one agent.
CREATE TABLE IF NOT EXISTS attempts (
    attempt_id        text        PRIMARY KEY,
    job_id            text        NOT NULL REFERENCES jobs (job_id) ON DELETE CASCADE,
    agent_id          text        NOT NULL,
    -- lease_token proves this attempt is the current one. It stops a stale
    -- attempt's reports from landing; it cannot stop that attempt from still
    -- reading disks or writing to the repository, which is why an expired
    -- lease fails the job rather than re-running it.
    lease_token_hash  bytea       NOT NULL,
    -- claim_request_key scopes claim idempotency to one request rather than to
    -- an agent: the same agent retrying an old request must not join a newer
    -- attempt.
    claim_request_key text        NOT NULL,
    -- Three separate times. lease_expires_at is authority; last_seen_at is
    -- liveness; last_progress_at is whether the work itself is moving. An
    -- agent that is alive but stuck looks different from one that is gone.
    lease_expires_at  timestamptz NOT NULL,
    last_seen_at      timestamptz NOT NULL DEFAULT now(),
    last_progress_at  timestamptz,
    -- last_seq orders reports within an attempt. An older seq is ignored.
    last_seq          bigint      NOT NULL DEFAULT 0,
    phase             text,
    processed_bytes   bigint      NOT NULL DEFAULT 0,
    outcome           text,
    message           text,
    -- result is the agent's report, kept so that a repeated report can be
    -- compared against what was already recorded.
    result            jsonb,
    superseded        boolean     NOT NULL DEFAULT false,
    created_at        timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz
);

DO $$
BEGIN
    IF to_regclass('attempts_claim_key') IS NULL THEN
        CREATE UNIQUE INDEX attempts_claim_key ON attempts (job_id, claim_request_key);
    END IF;
    IF to_regclass('attempts_job') IS NULL THEN
        CREATE INDEX attempts_job ON attempts (job_id);
    END IF;
END
$$;
