-- Development only: dropping the core schema destroys the ledger. The
-- migrate command refuses to run down migrations outside development.
DROP TABLE IF EXISTS audit_log, transaction_events, charges, transactions, app_api_keys, apps CASCADE;
DROP FUNCTION IF EXISTS tsp_audit_verify(), tsp_audit_chain(), tsp_audit_row_digest(audit_log, bytea),
  tsp_transactions_event(), tsp_transactions_insert(), tsp_transactions_guard(), tsp_charges_guard(),
  tsp_charges_insert(), tsp_api_keys_guard(), tsp_apps_guard(), tsp_forbid_truncate(), tsp_forbid_update(),
  tsp_forbid_delete(), tsp_set_updated_at() CASCADE;
DROP TYPE IF EXISTS audit_actor_type, txn_status, txn_channel, txn_kind, txn_direction, charge_status, app_status;
