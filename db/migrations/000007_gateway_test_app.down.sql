SET lock_timeout = '10s';

CREATE OR REPLACE FUNCTION tsp_apps_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.account_prefix <> OLD.account_prefix OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'apps.id, account_prefix and created_at are immutable'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;

DROP INDEX IF EXISTS apps_one_gateway_test;
ALTER TABLE apps DROP COLUMN IF EXISTS gateway_test;
