-- +goose Up

-- An agent's box (docs/workspace.md, The box): one per agent, its id the
-- box workspace's id. The container itself is sandboxd's; this row names it
-- for the API and holds the terminals.
CREATE TABLE boxes (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent      text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- A terminal: a tmux session in a box. Rows are never deleted: an ended
-- terminal keeps its record with ended_at and end_reason.
CREATE TABLE terminals (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id     uuid NOT NULL REFERENCES boxes (id),
    preset           text NOT NULL CHECK (preset IN ('shell', 'claude')),
    session          text NOT NULL UNIQUE,
    title            text NOT NULL,
    cwd              text NOT NULL DEFAULT '',
    cols             integer NOT NULL,
    rows             integer NOT NULL,
    created_by       text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    last_attached_at timestamptz,
    ended_at         timestamptz,
    end_reason       text CHECK (end_reason IN ('exit', 'killed', 'box_restarted')),
    exit_code        integer,
    CONSTRAINT terminals_ended CHECK ((ended_at IS NULL) = (end_reason IS NULL))
);
CREATE INDEX terminals_workspace ON terminals (workspace_id, created_at DESC);

-- Attach tickets: single-use, short-lived, bound to one terminal, one
-- person and one Origin. Only the ticket's sha256 is stored. Kept after use.
CREATE TABLE terminal_tickets (
    sha256      text PRIMARY KEY,
    terminal_id uuid NOT NULL REFERENCES terminals (id),
    person      text NOT NULL,
    origin      text NOT NULL,
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX terminal_tickets_person ON terminal_tickets (person, created_at DESC);

-- +goose Down
DROP TABLE terminal_tickets;
DROP TABLE terminals;
DROP TABLE boxes;
