#!/usr/bin/env bash
# Generates throwaway development secrets under deploy/dev-secrets/.
# These files are gitignored and must never be used outside a laptop.
# Staging/production secrets live in the SOPS-encrypted bundle
# (see docs/runbooks/secrets.md).
set -euo pipefail
dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/dev-secrets"
mkdir -p "$dir"
umask 077

gen() { head -c "$1" /dev/urandom | base64 | tr -d '\n=+/' | head -c "$2"; }

[ -f "$dir/pg_superuser_password" ] || gen 48 40 > "$dir/pg_superuser_password"
[ -f "$dir/pg_owner_password" ]     || gen 48 40 > "$dir/pg_owner_password"
[ -f "$dir/pg_app_password" ]       || gen 48 40 > "$dir/pg_app_password"
[ -f "$dir/api_key_pepper" ]        || gen 96 64 > "$dir/api_key_pepper"
[ -f "$dir/master_keys" ]           || printf '1:%s\n' "$(head -c 32 /dev/urandom | base64 | tr -d '\n')" > "$dir/master_keys"

port="${TSP_DEV_PG_PORT:-54329}"
printf 'postgres://talksasa_app:%s@127.0.0.1:%s/talksasa_pay?sslmode=disable\n' "$(cat "$dir/pg_app_password")" "$port" > "$dir/database_url"
printf 'postgres://talksasa_owner:%s@127.0.0.1:%s/talksasa_pay?sslmode=disable\n' "$(cat "$dir/pg_owner_password")" "$port" > "$dir/migrate_database_url"
# Inside the compose network the host is "postgres" on 5432.
sed "s/127.0.0.1:${port}/postgres:5432/" "$dir/database_url" > "$dir/database_url_compose"
sed "s/127.0.0.1:${port}/postgres:5432/" "$dir/migrate_database_url" > "$dir/migrate_database_url_compose"
# Owner-role URL to the maintenance DB, used by integration tests to create
# throwaway databases.
printf 'postgres://talksasa_owner:%s@127.0.0.1:%s/postgres?sslmode=disable\n' "$(cat "$dir/pg_owner_password")" "$port" > "$dir/test_admin_database_url"
chmod 600 "$dir"/*
# The postgres container reads these as its own uid; dev-only throwaways.
chmod 644 "$dir"/pg_owner_password "$dir"/pg_app_password "$dir"/pg_superuser_password
echo "dev secrets ready in $dir"
