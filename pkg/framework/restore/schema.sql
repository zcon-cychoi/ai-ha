-- Restore jobs. Applied by every command that opens the database: creates the
-- table only when it is absent and adds only the columns an existing table
-- lacks.
--
-- What is missing is read from the catalog, not left to "ADD COLUMN IF NOT
-- EXISTS". ALTER TABLE takes the strongest lock there is on the table before
-- it finds the column already there, and the whole file runs as one
-- transaction while the server may be serving claims and reports; the lock
-- would hold them up, and could meet theirs in the other order as a deadlock.
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

-- Progress and result reporting, added to tables created before reporting
-- existed; and repository_id, which replaced the repository column.
--
-- Restore jobs used to carry repository access, credentials included, in
-- plaintext. They name a registered repository instead. Dropping the column
-- keeps new jobs from storing credentials; PostgreSQL does not reclaim the
-- space the old values took until the table is rewritten (VACUUM FULL).
DO $$
DECLARE
    col record;
BEGIN
    FOR col IN SELECT * FROM (VALUES
        ('report_token_hash', 'bytea'),
        ('phase',             'text'),
        ('step',              'integer NOT NULL DEFAULT 0'),
        ('total_steps',       'integer NOT NULL DEFAULT 0'),
        ('current_item',      'text'),
        ('processed_bytes',   'bigint NOT NULL DEFAULT 0'),
        ('total_bytes',       'bigint NOT NULL DEFAULT 0'),
        -- last_seq orders reports. An older seq is ignored.
        ('last_seq',          'bigint NOT NULL DEFAULT 0'),
        ('last_progress_at',  'timestamptz'),
        ('restore_outcome',   'text'),
        ('message',           'text'),
        ('finished_at',       'timestamptz'),
        ('repository_id',     'text')
    ) AS c(name, definition)
    LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_attribute
                        WHERE attrelid = 'restore_jobs'::regclass AND attname = col.name AND NOT attisdropped) THEN
            EXECUTE format('ALTER TABLE restore_jobs ADD COLUMN %I %s', col.name, col.definition);
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_attribute
                WHERE attrelid = 'restore_jobs'::regclass AND attname = 'repository' AND NOT attisdropped) THEN
        ALTER TABLE restore_jobs DROP COLUMN repository;
    END IF;
END
$$;
