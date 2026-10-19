-- Backup job queue. Applied at startup; creates the tables only when they are
-- absent and does not alter existing ones.

-- A job is the logical work a user asked for. It outlives the attempts made to
-- carry it out, which is why the attempt is a separate row: reporting is tied
-- to an attempt, so a late report from a superseded one cannot move the job.
CREATE TABLE IF NOT EXISTS jobs (
    job_id             text        PRIMARY KEY,
    system_id          text        NOT NULL,
    kind               text        NOT NULL CHECK (kind IN ('backup')),
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

-- A job waiting for an agent is looked up by system on every long poll.
CREATE INDEX IF NOT EXISTS jobs_queued
    ON jobs (system_id, created_at) WHERE status = 'queued';

-- The sweeper walks jobs that can still time out.
CREATE INDEX IF NOT EXISTS jobs_unfinished
    ON jobs (status) WHERE status IN ('queued', 'claimed', 'running', 'cancelling');

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

CREATE UNIQUE INDEX IF NOT EXISTS attempts_claim_key
    ON attempts (job_id, claim_request_key);

CREATE INDEX IF NOT EXISTS attempts_job ON attempts (job_id);
