-- Development only (the migrate command refuses down migrations elsewhere).
DROP INDEX IF EXISTS transactions_created, transactions_unmatched, transactions_needs_review,
  charges_created, charges_status, charges_reference_trgm, audit_log_occurred;
DROP TABLE IF EXISTS admin_recovery_codes, admin_sessions, admin_invitations, admin_users CASCADE;
DROP FUNCTION IF EXISTS tsp_admin_recovery_codes_guard(), tsp_admin_sessions_guard(),
  tsp_admin_invitations_guard(), tsp_admin_users_guard() CASCADE;
DROP TYPE IF EXISTS admin_session_stage, admin_user_status, admin_role;
