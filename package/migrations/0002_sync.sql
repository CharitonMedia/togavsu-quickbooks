-- Sync state per core record. Each table holds QuickBooks ids, states, hashes and errors
-- keyed to the core record, never a copy of it.
CREATE TABLE customer_sync (
  id TEXT PRIMARY KEY,
  contact_id TEXT NOT NULL REFERENCES core.contacts (id) ON DELETE RESTRICT,
  qbo_display_name TEXT NOT NULL DEFAULT '',
  adopted BOOLEAN NOT NULL DEFAULT false,
  realm_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'synced', 'skipped', 'failed', 'drift', 'acknowledged', 'hidden')),
  skip_reason TEXT NOT NULL DEFAULT '',
  waiting_on TEXT NOT NULL DEFAULT '',
  qbo_id TEXT NOT NULL DEFAULT '',
  qbo_sync_token TEXT NOT NULL DEFAULT '',
  pushed_hash TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  attempts INT NOT NULL DEFAULT 0,
  next_attempt_at BIGINT NOT NULL DEFAULT 0,
  last_error_code TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  synced_at BIGINT,
  updated_at BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX customer_sync_contact ON customer_sync (contact_id);
CREATE INDEX customer_sync_due ON customer_sync (state, next_attempt_at);

CREATE TABLE invoice_sync (
  invoice_id TEXT PRIMARY KEY REFERENCES core.invoices (id) ON DELETE RESTRICT,
  customer_sync_id TEXT REFERENCES customer_sync (id),
  voided BOOLEAN NOT NULL DEFAULT false,
  credit_memo_qbo_id TEXT NOT NULL DEFAULT '',
  credit_memo_sync_token TEXT NOT NULL DEFAULT '',
  write_off_cents BIGINT NOT NULL DEFAULT 0,
  write_off_link_qbo_id TEXT NOT NULL DEFAULT '',
  realm_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'synced', 'skipped', 'failed', 'drift', 'acknowledged', 'hidden')),
  skip_reason TEXT NOT NULL DEFAULT '',
  waiting_on TEXT NOT NULL DEFAULT '',
  qbo_id TEXT NOT NULL DEFAULT '',
  qbo_sync_token TEXT NOT NULL DEFAULT '',
  pushed_hash TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  attempts INT NOT NULL DEFAULT 0,
  next_attempt_at BIGINT NOT NULL DEFAULT 0,
  last_error_code TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  synced_at BIGINT,
  updated_at BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX invoice_sync_due ON invoice_sync (state, next_attempt_at);

CREATE TABLE payment_sync (
  payment_id TEXT PRIMARY KEY REFERENCES core.payments (id) ON DELETE RESTRICT,
  trust_entry_id TEXT REFERENCES core.trust_ledger (id) ON DELETE RESTRICT,
  voided BOOLEAN NOT NULL DEFAULT false,
  realm_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'synced', 'skipped', 'failed', 'drift', 'acknowledged', 'hidden')),
  skip_reason TEXT NOT NULL DEFAULT '',
  waiting_on TEXT NOT NULL DEFAULT '',
  qbo_id TEXT NOT NULL DEFAULT '',
  qbo_sync_token TEXT NOT NULL DEFAULT '',
  pushed_hash TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  attempts INT NOT NULL DEFAULT 0,
  next_attempt_at BIGINT NOT NULL DEFAULT 0,
  last_error_code TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  synced_at BIGINT,
  updated_at BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX payment_sync_due ON payment_sync (state, next_attempt_at);

CREATE TABLE trust_entry_sync (
  trust_entry_id TEXT PRIMARY KEY REFERENCES core.trust_ledger (id) ON DELETE RESTRICT,
  linked_payment_id TEXT REFERENCES core.payments (id) ON DELETE RESTRICT,
  shape TEXT NOT NULL DEFAULT '' CHECK (shape IN ('', 'two_line', 'four_line', 'mirror')),
  realm_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'synced', 'skipped', 'failed', 'drift', 'acknowledged', 'hidden')),
  skip_reason TEXT NOT NULL DEFAULT '',
  waiting_on TEXT NOT NULL DEFAULT '',
  qbo_id TEXT NOT NULL DEFAULT '',
  qbo_sync_token TEXT NOT NULL DEFAULT '',
  pushed_hash TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  attempts INT NOT NULL DEFAULT 0,
  next_attempt_at BIGINT NOT NULL DEFAULT 0,
  last_error_code TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  synced_at BIGINT,
  updated_at BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX trust_entry_sync_due ON trust_entry_sync (state, next_attempt_at);

CREATE TABLE expense_sync (
  expense_id TEXT PRIMARY KEY REFERENCES core.expenses (id) ON DELETE RESTRICT,
  realm_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'synced', 'skipped', 'failed', 'drift', 'acknowledged', 'hidden')),
  skip_reason TEXT NOT NULL DEFAULT '',
  waiting_on TEXT NOT NULL DEFAULT '',
  qbo_id TEXT NOT NULL DEFAULT '',
  qbo_sync_token TEXT NOT NULL DEFAULT '',
  pushed_hash TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  attempts INT NOT NULL DEFAULT 0,
  next_attempt_at BIGINT NOT NULL DEFAULT 0,
  last_error_code TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  synced_at BIGINT,
  updated_at BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX expense_sync_due ON expense_sync (state, next_attempt_at);

-- Append-only: the module only inserts. One core record per row.
CREATE TABLE sync_log (
  id TEXT PRIMARY KEY,
  at BIGINT NOT NULL,
  realm_id TEXT NOT NULL DEFAULT '',
  contact_id TEXT REFERENCES core.contacts (id) ON DELETE RESTRICT,
  invoice_id TEXT REFERENCES core.invoices (id) ON DELETE RESTRICT,
  payment_id TEXT REFERENCES core.payments (id) ON DELETE RESTRICT,
  trust_entry_id TEXT REFERENCES core.trust_ledger (id) ON DELETE RESTRICT,
  expense_id TEXT REFERENCES core.expenses (id) ON DELETE RESTRICT,
  operation TEXT NOT NULL CHECK (operation IN ('create', 'update', 'void', 'delete', 'link', 'adopt', 'skip', 'acknowledge', 'retry_requested')),
  outcome TEXT NOT NULL CHECK (outcome IN ('ok', 'retry', 'failed', 'drift')),
  http_status INT NOT NULL DEFAULT 0,
  qbo_fault_code TEXT NOT NULL DEFAULT '',
  message TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  actor_user_id TEXT REFERENCES core.users (id) ON DELETE RESTRICT,
  CHECK (num_nonnulls(contact_id, invoice_id, payment_id, trust_entry_id, expense_id) = 1)
);
CREATE INDEX sync_log_at ON sync_log (at);
CREATE INDEX sync_log_contact ON sync_log (contact_id);
CREATE INDEX sync_log_invoice ON sync_log (invoice_id);
CREATE INDEX sync_log_payment ON sync_log (payment_id);
CREATE INDEX sync_log_trust_entry ON sync_log (trust_entry_id);
CREATE INDEX sync_log_expense ON sync_log (expense_id);
CREATE INDEX sync_log_actor ON sync_log (actor_user_id);

-- Reconcile runs, nightly and manual.
CREATE TABLE runs (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('nightly', 'manual')),
  started_at BIGINT NOT NULL,
  finished_at BIGINT,
  examined INT NOT NULL DEFAULT 0,
  queued INT NOT NULL DEFAULT 0,
  failed INT NOT NULL DEFAULT 0,
  drift INT NOT NULL DEFAULT 0,
  started_by TEXT REFERENCES core.users (id) ON DELETE RESTRICT
);

-- Alerts sent, so each interval sends at most one.
CREATE TABLE alerts_sent (
  id TEXT PRIMARY KEY,
  at BIGINT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('failures', 'connection', 'reconnect_soon')),
  item_count INT NOT NULL DEFAULT 0
);
