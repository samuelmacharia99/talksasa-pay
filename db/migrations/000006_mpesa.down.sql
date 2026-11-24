-- Development only (the migrate command refuses down migrations elsewhere).
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_subject_type_check;
ALTER TABLE events ADD CONSTRAINT events_subject_type_check
  CHECK (subject_type IN ('payment_request', 'transaction', 'payer')) NOT VALID; -- stk_push events may exist
DROP TABLE IF EXISTS mpesa_callbacks, stk_pushes CASCADE;
DROP FUNCTION IF EXISTS tsp_mpesa_callbacks_guard() CASCADE;
DROP FUNCTION IF EXISTS tsp_stk_pushes_guard() CASCADE;
DROP TYPE IF EXISTS mpesa_callback_kind;
DROP TYPE IF EXISTS stk_push_status;
