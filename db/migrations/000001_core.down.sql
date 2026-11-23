-- Development only: dropping the core schema destroys the ledger. The
-- migrate command refuses to run down migrations outside development.
DROP TABLE IF EXISTS audit_log, allocations, allocation_batches, transaction_events, transactions,
  payment_requests, payer_phones, payer_accounts, product_api_keys, product_account_counters, products CASCADE;
DROP FUNCTION IF EXISTS tsp_audit_verify(), tsp_audit_chain(), tsp_audit_row_digest(audit_log, bytea),
  tsp_allocations_balanced(), tsp_allocation_batches_balanced(), tsp_check_batch_balanced(uuid), tsp_allocations_apply(), tsp_allocation_batches_insert(),
  tsp_transactions_event(), tsp_transactions_guard(), tsp_payment_requests_guard(), tsp_payment_requests_insert(),
  tsp_payer_accounts_guard(), tsp_payer_accounts_insert(), tsp_api_keys_guard(), tsp_counter_guard(),
  tsp_products_guard(), tsp_allocating(), tsp_forbid_truncate(), tsp_forbid_update(), tsp_forbid_delete(),
  tsp_set_updated_at() CASCADE;
DROP TYPE IF EXISTS audit_actor_type, allocation_batch_kind, txn_status, txn_channel, txn_kind, txn_direction,
  payment_request_status, payer_status, product_status;
