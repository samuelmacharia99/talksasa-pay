-- Development only (the migrate command refuses down migrations elsewhere).
DROP TABLE IF EXISTS webhook_attempts, webhook_deliveries, events CASCADE;
DROP FUNCTION IF EXISTS tsp_webhook_deliveries_guard() CASCADE;
DROP TYPE IF EXISTS webhook_delivery_status;
