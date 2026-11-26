-- The Settings page can send a real test payment (admins, KES 1-1,000).
-- Each test is a real charge on one reserved internal app, "Gateway test",
-- so it runs the whole flow (prompt, callback, receipt, ledger) and its
-- money is reported under that app, never under a real one. The flag marks
-- that app; at most one app carries it and it can never be moved.

SET lock_timeout = '10s';

ALTER TABLE apps ADD COLUMN gateway_test boolean NOT NULL DEFAULT false;

CREATE UNIQUE INDEX apps_one_gateway_test ON apps ((true)) WHERE gateway_test;

CREATE OR REPLACE FUNCTION tsp_apps_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.account_prefix <> OLD.account_prefix OR NEW.created_at <> OLD.created_at
     OR NEW.gateway_test <> OLD.gateway_test THEN
    RAISE EXCEPTION 'apps.id, account_prefix, created_at and gateway_test are immutable'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
