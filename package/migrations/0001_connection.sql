-- The one QuickBooks connection. No column holds a token: the access and refresh tokens
-- live only in the core secret store (qbo/access_token, qbo/refresh_token).
CREATE TABLE connection (
  id SMALLINT PRIMARY KEY CHECK (id = 1),
  environment TEXT NOT NULL DEFAULT 'sandbox' CHECK (environment IN ('sandbox', 'production')),
  realm_id TEXT NOT NULL DEFAULT '',
  company_name TEXT NOT NULL DEFAULT '',
  access_expires_at BIGINT NOT NULL DEFAULT 0,
  refresh_expires_at BIGINT NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'disconnected' CHECK (state IN ('disconnected', 'connected', 'reconnect_needed')),
  connected_by TEXT REFERENCES core.users (id) ON DELETE RESTRICT,
  connected_at BIGINT NOT NULL DEFAULT 0,
  first_connected_on TEXT NOT NULL DEFAULT '',
  last_refresh_at BIGINT NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT ''
);

-- One-use OAuth states, each bound to the admin who pressed Connect.
CREATE TABLE oauth_states (
  state TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES core.users (id) ON DELETE RESTRICT,
  redirect_uri TEXT NOT NULL DEFAULT '',
  expires_at BIGINT NOT NULL,
  used_at BIGINT
);

-- Events the core delivered, each recorded once.
CREATE TABLE received_events (
  event_id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  record_id TEXT NOT NULL,
  received_at BIGINT NOT NULL
);

-- A QuickBooks bank account per Togavsu trust account, in place of the default. Its link
-- to core.trust_accounts is migration 0004, which waits on CORE-22 (pending.yaml).
CREATE TABLE trust_bank_map (
  trust_account_id TEXT PRIMARY KEY,
  qbo_account TEXT NOT NULL,
  set_by TEXT REFERENCES core.users (id) ON DELETE RESTRICT,
  set_at BIGINT NOT NULL DEFAULT 0
);
