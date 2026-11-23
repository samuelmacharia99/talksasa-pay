#!/usr/bin/env bash
# Runs once when the dev/test Postgres volume is first created.
# Creates the schema-owner role (used only by migrations) and the
# least-privilege application role (no DELETE/TRUNCATE on ledger tables).
set -euo pipefail

owner_pw="$(cat /run/secrets/pg_owner_password)"
app_pw="$(cat /run/secrets/pg_app_password)"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
  -v owner_pw="$owner_pw" -v app_pw="$app_pw" <<'SQL'
CREATE ROLE talksasa_owner LOGIN PASSWORD :'owner_pw' CREATEDB;
CREATE ROLE talksasa_app   LOGIN PASSWORD :'app_pw' NOSUPERUSER NOCREATEDB NOCREATEROLE;
-- Lets integration tests run as the app role on throwaway databases (dev only).
GRANT talksasa_app TO talksasa_owner;
CREATE DATABASE talksasa_pay OWNER talksasa_owner;
\c talksasa_pay
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
ALTER SCHEMA public OWNER TO talksasa_owner;
SQL
