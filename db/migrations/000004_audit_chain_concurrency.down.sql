-- Development only (the migrate command refuses down migrations elsewhere).
CREATE OR REPLACE FUNCTION tsp_audit_chain() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  last audit_log%ROWTYPE;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtext('talksasa.audit_log'));
  SELECT * INTO last FROM audit_log ORDER BY id DESC LIMIT 1;
  IF FOUND THEN
    NEW.prev_id := last.id;
    NEW.prev_hash := last.hash;
  ELSE
    NEW.prev_id := NULL;
    NEW.prev_hash := NULL;
  END IF;
  NEW.hash := tsp_audit_row_digest(NEW, NEW.prev_hash);
  RETURN NEW;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'talksasa_app') THEN
    EXECUTE format('REVOKE USAGE ON SEQUENCE %s FROM talksasa_app', pg_get_serial_sequence('audit_log', 'id'));
  END IF;
END $$;
