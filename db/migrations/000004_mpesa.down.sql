-- Development only (the migrate command refuses down migrations elsewhere).
DROP TABLE IF EXISTS mpesa_callbacks, stk_pushes CASCADE;
DROP FUNCTION IF EXISTS tsp_mpesa_callbacks_guard() CASCADE;
DROP FUNCTION IF EXISTS tsp_stk_pushes_guard() CASCADE;
DROP TYPE IF EXISTS mpesa_callback_kind;
DROP TYPE IF EXISTS stk_push_status;
