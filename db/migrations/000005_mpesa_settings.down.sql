-- Development only (the migrate command refuses down migrations elsewhere).
DROP TABLE IF EXISTS mpesa_settings CASCADE;
DROP FUNCTION IF EXISTS tsp_mpesa_settings_guard() CASCADE;
