-- Published view: whether an invoice is in the books, for other modules (the client
-- portal) under views:quickbooks.invoice_qbo:read.
CREATE VIEW invoice_qbo AS SELECT invoice_id, state, synced_at FROM invoice_sync;
