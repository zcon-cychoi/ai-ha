-- Redundancy (HA) groups. Applied on every start: tables are created when
-- absent, and the columns added later are added only when missing, so that a
-- database made by an earlier release keeps its rows.

-- An HA group pairs a primary and a standby system around one replicated
-- volume and the service that runs on it.
--
-- Two things are kept apart on purpose. state governs what may be done next
-- and moves only by a flow, an operator, or a judgement that has lasted.
-- verdict is derived anew at every evaluation. A broken replication link is a
-- verdict of error on a group whose state is degraded; collapsing the two would
-- let a momentary reading change what a switch is allowed to do.
CREATE TABLE IF NOT EXISTS ha_groups (
    group_id    text        PRIMARY KEY,
    name        text        NOT NULL,
    state       text        NOT NULL
        CHECK (state IN ('registered', 'configuring', 'syncing', 'protected',
                         'degraded', 'failover_pending', 'switching', 'error')),
    -- unknown until the first evaluation.
    verdict     text        NOT NULL DEFAULT 'unknown'
        CHECK (verdict IN ('unknown', 'ok', 'attention', 'error')),
    reasons     jsonb       NOT NULL DEFAULT '[]',
    evaluated_at timestamptz,
    -- The replication resource and the service definition. Held as JSON so
    -- the shape can grow without a migration, as the job spec is. Neither
    -- holds the peers' shared secret, which is never stored with the group.
    resource    jsonb       NOT NULL,
    service     jsonb       NOT NULL,
    -- Where clients reach the service, recorded so that a switch can say what
    -- to repoint.
    service_address text,
    -- The last evaluation at which both nodes reported the link connected,
    -- both disks up to date and nothing out of sync. A failover cannot prove
    -- the standby is current; this is the last moment it was known to be.
    last_sync_confirmed_at timestamptz,
    -- When the conditions that only count once they last were first seen.
    -- Cleared when they end.
    link_down_since        timestamptz,
    primary_failing_since  timestamptz,
    -- The last sequence number given to one of this group's events. It is
    -- advanced under this row's lock, so that a group's events commit in
    -- sequence order and a reader resuming after the last one it saw misses
    -- none. A database sequence would not do: numbers are taken in one order
    -- and committed in another.
    event_seq   bigint      NOT NULL DEFAULT 0,
    created_by  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ha_groups_name_unique UNIQUE (name)
);

-- The two systems of a group. system_id is the key: a system belongs to at
-- most one group, because a node can hold only one role.
CREATE TABLE IF NOT EXISTS ha_nodes (
    system_id   text        NOT NULL,
    group_id    text        NOT NULL REFERENCES ha_groups (group_id) ON DELETE CASCADE,
    role        text        NOT NULL CHECK (role IN ('primary', 'standby')),
    -- The node's private address, used for replication and for the
    -- standby's probe of the primary.
    address     text        NOT NULL,
    -- Set when an operator has recorded that this node was stopped so that it
    -- can no longer write. A group with a fenced node is not protected.
    fenced      boolean     NOT NULL DEFAULT false,
    -- The latest report this node sent under its current role, and when it
    -- arrived. received_at is the only liveness signal redundancy uses: an
    -- agent's last long poll can be hours old while it runs a backup.
    report              jsonb,
    report_collected_at timestamptz,
    report_received_at  timestamptz,
    agent_id    text,
    CONSTRAINT ha_nodes_system_unique PRIMARY KEY (system_id),
    CONSTRAINT ha_nodes_group_role_unique UNIQUE (group_id, role)
);

-- What happened to a group, in order. seq is the group's own sequence and the
-- one to resume from; event_id only names a row.
CREATE TABLE IF NOT EXISTS ha_events (
    event_id    bigserial   PRIMARY KEY,
    group_id    text        NOT NULL REFERENCES ha_groups (group_id) ON DELETE CASCADE,
    seq         bigint      NOT NULL,
    kind        text        NOT NULL,
    from_value  text,
    to_value    text,
    reasons     jsonb       NOT NULL DEFAULT '[]',
    at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ha_events_group_seq_unique UNIQUE (group_id, seq)
);

-- Added with replication setup (plan/ha-roadmap.md H3b).
--
-- Every command that opens the database applies this file, while the server
-- may be recording reports. "ALTER TABLE ... IF NOT EXISTS" and "CREATE INDEX
-- IF NOT EXISTS" lock the table before they find there is nothing to do, and
-- the whole file runs as one transaction, so the locks pile up and can meet a
-- report's in the other order. The changes to existing tables are therefore
-- made only when they are missing, decided from the catalog, which locks no
-- table.
--
-- disk is the node's data disk as a /dev/disk/by-id path. Each node has its
-- own: the by-id name differs between nodes on most clouds. It is empty for a
-- group that adopts a resource configured by hand, which cannot be set up.
--
-- node_id is the node's number within the replication resource. It belongs to
-- the system, not the role, because the metadata on disk is laid out by it and
-- a switch must not change it. Groups made before it existed are numbered by
-- the role they were registered with, which is what this release assigns too.
--
-- replication_secret is the peers' shared secret, sealed with the framework's
-- key and bound to the group. It is made by the first setup and kept, so that
-- a repeated setup writes the same resource file and changes nothing on a node
-- already set up.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                    WHERE attrelid = 'ha_nodes'::regclass AND attname = 'disk' AND NOT attisdropped) THEN
        ALTER TABLE ha_nodes ADD COLUMN disk text;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                    WHERE attrelid = 'ha_nodes'::regclass AND attname = 'node_id' AND NOT attisdropped) THEN
        ALTER TABLE ha_nodes ADD COLUMN node_id smallint;
        UPDATE ha_nodes SET node_id = CASE role WHEN 'primary' THEN 0 ELSE 1 END;
    END IF;
    IF to_regclass('ha_nodes_group_node_id') IS NULL THEN
        CREATE UNIQUE INDEX ha_nodes_group_node_id ON ha_nodes (group_id, node_id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                    WHERE attrelid = 'ha_groups'::regclass AND attname = 'replication_secret' AND NOT attisdropped) THEN
        ALTER TABLE ha_groups ADD COLUMN replication_secret bytea;
    END IF;
END
$$;

-- An HA operation on a group: setting replication up or taking it down. It
-- runs as numbered steps, each one job per node it acts on, and moves to the
-- next step only once the step's jobs completed and the nodes' own reports
-- agree.
CREATE TABLE IF NOT EXISTS ha_flows (
    flow_id      text        PRIMARY KEY,
    group_id     text        NOT NULL REFERENCES ha_groups (group_id) ON DELETE CASCADE,
    kind         text        NOT NULL CHECK (kind IN ('setup', 'teardown')),
    status       text        NOT NULL CHECK (status IN ('running', 'completed', 'failed')),
    step         integer     NOT NULL,
    message      text,
    requested_by text,
    started_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz
);

-- One operation at a time per group. Created only when missing, for the
-- reason above.
DO $$
BEGIN
    IF to_regclass('ha_flows_one_running') IS NULL THEN
        CREATE UNIQUE INDEX ha_flows_one_running ON ha_flows (group_id) WHERE status = 'running';
    END IF;
END
$$;

-- The jobs of a flow's steps. The key is what keeps a step from being issued
-- twice: a framework that stops after creating a step's job finds it here on
-- restart instead of creating another.
CREATE TABLE IF NOT EXISTS ha_flow_jobs (
    flow_id   text    NOT NULL REFERENCES ha_flows (flow_id) ON DELETE CASCADE,
    step      integer NOT NULL,
    system_id text    NOT NULL,
    job_id    text    NOT NULL,
    CONSTRAINT ha_flow_jobs_step_unique PRIMARY KEY (flow_id, step, system_id),
    CONSTRAINT ha_flow_jobs_job_unique UNIQUE (job_id)
);
