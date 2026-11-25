-- Integration plumbing for apps.
--
--   * events: transactional outbox. Every change an app must hear about
--     (charge succeeded/needs review/expired/cancelled, prompt sent/failed,
--     payment received, transaction reversed) is written here IN THE SAME
--     TRANSACTION as the change itself, so a committed change can never miss
--     its event. Rows are immutable and never deleted.
--   * txid: the writing transaction's id. The /v1/events feed only returns
--     events whose transaction is older than every still-running one
--     (pg_snapshot_xmin), so a slow transaction that commits "late" can never
--     be skipped by a consumer's cursor. Ordering is (txid, id).
--   * webhook_deliveries / webhook_attempts: delivery state and attempt log.
--     Delivery itself runs on River jobs (Go), inserted in the same
--     transaction as the event.

SET lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- Events (outbox + pull feed)
-- ---------------------------------------------------------------------------
CREATE TABLE events (
  id            uuid PRIMARY KEY,
  txid          bigint NOT NULL DEFAULT (pg_current_xact_id()::text)::bigint,
  app_id        uuid NOT NULL REFERENCES apps (id),
  type          text NOT NULL CHECK (type ~ '^[a-z_]{3,40}\.[a-z_]{3,40}$'),
  subject_type  text NOT NULL CHECK (subject_type IN ('charge', 'transaction', 'stk_push')),
  subject_id    uuid NOT NULL,
  data          jsonb NOT NULL CHECK (jsonb_typeof(data) = 'object' AND octet_length(data::text) <= 65536),
  created_at    timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE events IS 'Append-only outbox of app-visible changes. Written in the same transaction as the change.';
CREATE INDEX events_feed ON events (app_id, txid, id);
CREATE INDEX events_subject ON events (app_id, subject_id, created_at);
CREATE TRIGGER events_no_update BEFORE UPDATE ON events FOR EACH ROW EXECUTE FUNCTION tsp_forbid_update();
CREATE TRIGGER events_no_delete BEFORE DELETE ON events FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();

-- ---------------------------------------------------------------------------
-- Webhook delivery state
-- ---------------------------------------------------------------------------
CREATE TYPE webhook_delivery_status AS ENUM ('pending', 'delivered', 'failed', 'skipped');

CREATE TABLE webhook_deliveries (
  event_id          uuid PRIMARY KEY REFERENCES events (id),
  app_id            uuid NOT NULL REFERENCES apps (id),
  status            webhook_delivery_status NOT NULL DEFAULT 'pending',
  attempts          integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  last_attempt_at   timestamptz,
  last_status_code  integer CHECK (last_status_code IS NULL OR last_status_code BETWEEN 100 AND 599),
  last_error        text CHECK (last_error IS NULL OR length(last_error) <= 500),
  delivered_at      timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);
CREATE INDEX webhook_deliveries_app ON webhook_deliveries (app_id, created_at DESC);
CREATE INDEX webhook_deliveries_failed ON webhook_deliveries (app_id, created_at DESC) WHERE status = 'failed';

CREATE FUNCTION tsp_webhook_deliveries_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.event_id <> OLD.event_id OR NEW.app_id <> OLD.app_id OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'webhook_deliveries identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.attempts < OLD.attempts THEN
    RAISE EXCEPTION 'webhook attempt counter cannot go backwards' USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  -- delivered is final; failed may be re-queued by an admin (back to pending).
  IF OLD.status = 'delivered' AND NEW.status <> 'delivered' THEN
    RAISE EXCEPTION 'a delivered webhook cannot change status' USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER webhook_deliveries_guard BEFORE UPDATE ON webhook_deliveries FOR EACH ROW EXECUTE FUNCTION tsp_webhook_deliveries_guard();
CREATE TRIGGER webhook_deliveries_updated_at BEFORE UPDATE ON webhook_deliveries FOR EACH ROW EXECUTE FUNCTION tsp_set_updated_at();
CREATE TRIGGER webhook_deliveries_no_delete BEFORE DELETE ON webhook_deliveries FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();

-- One row per HTTP attempt. Diagnostic only (no money meaning), so the
-- retention job may delete old rows.
CREATE TABLE webhook_attempts (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  event_id      uuid NOT NULL REFERENCES webhook_deliveries (event_id),
  attempt       integer NOT NULL CHECK (attempt >= 1),
  attempted_at  timestamptz NOT NULL DEFAULT now(),
  url_host      text NOT NULL CHECK (length(url_host) <= 255),
  status_code   integer CHECK (status_code IS NULL OR status_code BETWEEN 100 AND 599),
  latency_ms    integer NOT NULL CHECK (latency_ms >= 0),
  error         text CHECK (error IS NULL OR length(error) <= 500),
  response_body text CHECK (response_body IS NULL OR length(response_body) <= 1024),
  UNIQUE (event_id, attempt)
);
CREATE INDEX webhook_attempts_at ON webhook_attempts (attempted_at);
CREATE TRIGGER webhook_attempts_no_update BEFORE UPDATE ON webhook_attempts FOR EACH ROW EXECUTE FUNCTION tsp_forbid_update();

-- ---------------------------------------------------------------------------
-- Privileges (River's own tables are granted by the migrate command after
-- River's migrations run).
-- ---------------------------------------------------------------------------
REVOKE ALL ON events, webhook_deliveries, webhook_attempts FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'talksasa_app') THEN
    GRANT SELECT, INSERT ON events TO talksasa_app;
    GRANT SELECT, INSERT, UPDATE ON webhook_deliveries TO talksasa_app;
    GRANT SELECT, INSERT, DELETE ON webhook_attempts TO talksasa_app;
  ELSE
    RAISE WARNING 'role talksasa_app does not exist; skipping grants';
  END IF;
END $$;
