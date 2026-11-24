-- Development only (the migrate command refuses down migrations elsewhere).
DROP INDEX IF EXISTS transactions_created, transactions_suspense, transactions_needs_review,
  payment_requests_created, payment_requests_status, payer_accounts_created,
  payer_accounts_name_trgm, payer_accounts_ext_ref_trgm, audit_log_occurred;
DROP TABLE IF EXISTS admin_recovery_codes, admin_sessions, admin_invitations, admin_users CASCADE;
DROP FUNCTION IF EXISTS tsp_admin_recovery_codes_guard(), tsp_admin_sessions_guard(),
  tsp_admin_invitations_guard(), tsp_admin_users_guard() CASCADE;
DROP TYPE IF EXISTS admin_session_stage, admin_user_status, admin_role;
