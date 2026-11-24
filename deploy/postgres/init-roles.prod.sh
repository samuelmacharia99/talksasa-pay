#!/usr/bin/env bash
# Runs once, when the staging/production Postgres volume is first created.
# Schema owner (migrations only) and the least-privilege application role.
# Unlike the dev script: no CREATEDB, no role membership, TLS-only logins.
set -euo pipefail

owner_pw="$(cat /run/secrets/pg_owner_password)"
app_pw="$(cat /run/secrets/pg_app_password)"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
  -v owner_pw="$owner_pw" -v app_pw="$app_pw" <<'SQL'
CREATE ROLE talksasa_owner LOGIN PASSWORD :'owner_pw' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE talksasa_app   LOGIN PASSWORD :'app_pw'   NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE DATABASE talksasa_pay OWNER talksasa_owner;
REVOKE ALL ON DATABASE talksasa_pay FROM PUBLIC;
GRANT CONNECT ON DATABASE talksasa_pay TO talksasa_app;
\c talksasa_pay
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
ALTER SCHEMA public OWNER TO talksasa_owner;
SQL

# Network logins must use TLS + SCRAM; local socket stays for the superuser
# (backups via `docker compose exec`).
cat > "$PGDATA/pg_hba.conf" <<'HBA'
local   all  postgres                 peer
local   all  all                      reject
hostssl all  postgres  all            reject
hostssl talksasa_pay talksasa_owner,talksasa_app all scram-sha-256
host    all  all       all            reject
HBA
