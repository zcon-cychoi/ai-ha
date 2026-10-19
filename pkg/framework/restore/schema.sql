-- Restore jobs. Applied at startup; creates the table only when it is absent
-- and adds only the columns an existing table lacks.
CREATE TABLE IF NOT EXISTS restore_jobs (
    restore_id            text        PRIMARY KEY,
    recovery_id           text        NOT NULL UNIQUE,
    claim_token_hash      bytea       NOT NULL,
    claim_expires_at      timestamptz NOT NULL,
    status                text        NOT NULL,
    plan                  jsonb       NOT NULL,
    -- The registered repository holding the set. Its credentials stay in the
    -- repositories table, encrypted; they are decrypted for each claim.
    repository_id         text,
    claim_request_key     text,
    claimed_at            timestamptz,
    redelivery_expires_at timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now()
);

-- Progress and result reporting. Added to tables created before reporting
-- existed, so each column is added only when it is missing.
ALTER TABLE restore_jobs
    ADD COLUMN IF NOT EXISTS report_token_hash bytea,
    ADD COLUMN IF NOT EXISTS phase             text,
    ADD COLUMN IF NOT EXISTS step              integer     NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS total_steps       integer     NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS current_item      text,
    ADD COLUMN IF NOT EXISTS processed_bytes   bigint      NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS total_bytes       bigint      NOT NULL DEFAULT 0,
    -- last_seq orders reports. An older seq is ignored.
    ADD COLUMN IF NOT EXISTS last_seq          bigint      NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_progress_at  timestamptz,
    ADD COLUMN IF NOT EXISTS restore_outcome   text,
    ADD COLUMN IF NOT EXISTS message           text,
    ADD COLUMN IF NOT EXISTS finished_at       timestamptz;

-- Restore jobs used to carry repository access, credentials included, in
-- plaintext. They name a registered repository instead. Dropping the column
-- keeps new jobs from storing credentials; PostgreSQL does not reclaim the
-- space the old values took until the table is rewritten (VACUUM FULL).
ALTER TABLE restore_jobs ADD COLUMN IF NOT EXISTS repository_id text;
ALTER TABLE restore_jobs DROP COLUMN IF EXISTS repository;
