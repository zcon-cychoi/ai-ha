-- What each protected system is made of, as its agent reports it. Applied at
-- startup; creates the table only when it is absent and does not alter an
-- existing one.

-- One row per system. The framework needs the current configuration in order
-- to choose backup targets; the history of a system's layout is not kept here,
-- because every backup manifest already records the layout as it was at that
-- capture, which is the form a restore needs it in.
CREATE TABLE IF NOT EXISTS system_inventories (
    system_id    text        PRIMARY KEY,
    -- The agent that last reported. Recorded so that an operator looking at a
    -- surprising inventory can tell which installation produced it.
    agent_id     text        NOT NULL,
    -- Separated on purpose. collected_at is when the agent read the system;
    -- reported_at is when the framework heard it. An agent reporting
    -- punctually from a stale reading is a different fault from one that has
    -- gone quiet, and only the first can put a vanished partition into a job.
    collected_at timestamptz NOT NULL,
    reported_at  timestamptz NOT NULL DEFAULT now(),
    -- False when the agent could not account for every filesystem the system
    -- needs. Backup targets are never chosen automatically from such a row:
    -- the resulting set would be missing a filesystem, the backup would
    -- succeed, and only the restore would fail.
    complete     boolean     NOT NULL,
    -- The report as received. Held as JSON so the shape can grow without a
    -- migration, as the job spec is.
    inventory    jsonb       NOT NULL
);
