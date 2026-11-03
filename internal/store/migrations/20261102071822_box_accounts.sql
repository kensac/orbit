-- +goose Up

-- A Claude account in a box (docs/workspace.md, Accounts): a directory on
-- its own named volume, orbit-box-<agent>-acct-<slot>, used as the
-- CLAUDE_CONFIG_DIR of the terminals that name it. This row is what orbit
-- knows of it, read from the box by a fixed script: never a token, which
-- lives only in the account's volume. Rows are never deleted.
CREATE TABLE box_accounts (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id       uuid NOT NULL REFERENCES boxes (id),
    -- The volume's and the directory's name: 12 hex digits of the id.
    slot               text NOT NULL UNIQUE CHECK (slot ~ '^[0-9a-f]{12}$'),
    label              text NOT NULL CHECK (length(label) BETWEEN 1 AND 100),
    logged_in          boolean NOT NULL DEFAULT false,
    email              text,
    account_uuid       text,
    org_id             text,
    org_name           text,
    subscription_type  text,
    rate_limit_tier    text,
    access_expires_at  timestamptz,
    refresh_expires_at timestamptz,
    -- When the fields above were last read from the box.
    checked_at         timestamptz,
    created_by         text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT box_accounts_label UNIQUE (workspace_id, label)
);

-- The account a terminal runs under; null is the box's own profile.
ALTER TABLE terminals ADD COLUMN account_id uuid REFERENCES box_accounts (id);

-- login: `claude auth login` under an account's directory.
ALTER TABLE terminals DROP CONSTRAINT terminals_preset_check;
ALTER TABLE terminals ADD CONSTRAINT terminals_preset_check CHECK (preset IN ('shell', 'claude', 'login'));
ALTER TABLE terminals ADD CONSTRAINT terminals_login_account CHECK (preset <> 'login' OR account_id IS NOT NULL);

-- +goose Down
ALTER TABLE terminals DROP CONSTRAINT terminals_login_account;
ALTER TABLE terminals DROP CONSTRAINT terminals_preset_check;
ALTER TABLE terminals ADD CONSTRAINT terminals_preset_check CHECK (preset IN ('shell', 'claude'));
ALTER TABLE terminals DROP COLUMN account_id;
DROP TABLE box_accounts;
