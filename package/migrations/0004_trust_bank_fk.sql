-- Links each trust bank mapping to its Togavsu trust account. It waits on CORE-22, which
-- makes core.trust_accounts a record a module can link to; until then the core refuses
-- this file and pending.yaml holds it back whole, and the trust-bank-map action checks the
-- account against the core's trust account list instead.
ALTER TABLE trust_bank_map ADD CONSTRAINT trust_bank_map_trust_account_fk
  FOREIGN KEY (trust_account_id) REFERENCES core.trust_accounts (id) ON DELETE RESTRICT;
