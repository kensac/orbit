-- +goose Up

-- mcp-login: `claude mcp login --no-browser <server>` under an account, for
-- an HTTP server or a claude.ai connector (docs/api.md, Accounts); the
-- server it logs in to is mcp_server.
ALTER TABLE terminals ADD COLUMN mcp_server text CHECK (length(mcp_server) BETWEEN 1 AND 100);
ALTER TABLE terminals DROP CONSTRAINT terminals_preset_check;
ALTER TABLE terminals ADD CONSTRAINT terminals_preset_check CHECK (preset IN ('shell', 'claude', 'login', 'mcp-login'));
ALTER TABLE terminals DROP CONSTRAINT terminals_login_account;
ALTER TABLE terminals ADD CONSTRAINT terminals_login_account CHECK (preset NOT IN ('login', 'mcp-login') OR account_id IS NOT NULL);
ALTER TABLE terminals ADD CONSTRAINT terminals_mcp_server CHECK ((preset = 'mcp-login') = (mcp_server IS NOT NULL));

-- +goose Down
ALTER TABLE terminals DROP CONSTRAINT terminals_mcp_server;
ALTER TABLE terminals DROP CONSTRAINT terminals_login_account;
ALTER TABLE terminals ADD CONSTRAINT terminals_login_account CHECK (preset <> 'login' OR account_id IS NOT NULL);
ALTER TABLE terminals DROP CONSTRAINT terminals_preset_check;
ALTER TABLE terminals ADD CONSTRAINT terminals_preset_check CHECK (preset IN ('shell', 'claude', 'login'));
ALTER TABLE terminals DROP COLUMN mcp_server;
