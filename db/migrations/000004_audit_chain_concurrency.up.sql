-- Fix: audit_log appends failed under concurrent writers.
--
-- The identity default for audit_log.id is drawn when the row is formed,
-- BEFORE the chain trigger takes its advisory lock. Two concurrent writers
-- can therefore commit in the opposite order to their ids, after which
-- "ORDER BY id DESC LIMIT 1" no longer finds the chain head and the next
-- append collides on audit_log_prev_id_key (unique_violation). Observed with
-- 8 concurrent writers: most appends failed.
--
-- Now the id is re-drawn while holding the lock, so id order equals chain
-- order and the highest id is always the head. A chain written by the old
-- trigger may already be out of order, so if the highest id already has a
-- successor the head is found by following the links instead (rare, and only
-- until the first append under this trigger).

SET lock_timeout = '10s';

CREATE OR REPLACE FUNCTION tsp_audit_chain() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  last audit_log%ROWTYPE;
BEGIN
  -- Serialise appends so the chain stays linear. Under READ COMMITTED the
  -- SELECT below sees the row committed by whoever held the lock before us.
  PERFORM pg_advisory_xact_lock(hashtext('talksasa.audit_log'));
  NEW.id := nextval(pg_get_serial_sequence('audit_log', 'id'));
  SELECT * INTO last FROM audit_log ORDER BY id DESC LIMIT 1;
  IF FOUND AND EXISTS (SELECT 1 FROM audit_log WHERE prev_id = last.id) THEN
    SELECT a.* INTO last FROM audit_log a
     WHERE NOT EXISTS (SELECT 1 FROM audit_log b WHERE b.prev_id = a.id)
     ORDER BY a.id DESC LIMIT 1;
  END IF;
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

-- The trigger runs as the inserting role, which now calls nextval itself.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'talksasa_app') THEN
    EXECUTE format('GRANT USAGE ON SEQUENCE %s TO talksasa_app', pg_get_serial_sequence('audit_log', 'id'));
  ELSE
    RAISE WARNING 'role talksasa_app does not exist; skipping grants';
  END IF;
END $$;
