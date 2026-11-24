# Runbook: deploying Talksasa Pay

One script, run from a workstation: `scripts/deploy.sh`. It tests, builds the
three images (Go service, admin panel, pay page), ships them to the host over
SSH, and runs `deploy/host/apply.sh` there. Nothing is built on the server and
the server never needs the source code or GitHub access.

```
workstation                                   deploy host (Ubuntu + Docker)
  make verify                                   /etc/talksasa-pay/<env>.env      settings
  docker build  x3  ──docker save | ssh──►      /etc/talksasa-pay/age.key        decrypts secrets
  release bundle    ──tar | ssh──────────►      /opt/talksasa-pay/<env>/releases/<version>/
                                                /run/talksasa-pay/<env>/secrets/ tmpfs, 0400
                                                Caddy :80/:443 ─► pay / admin / api
```

## Every deploy
```sh
scripts/deploy.sh staging --host deploy@staging-host
scripts/deploy.sh production --host deploy@prod-host     # asks you to type "production"
scripts/deploy.sh status production --host deploy@prod-host
scripts/deploy.sh rollback production --host deploy@prod-host
scripts/deploy.sh local                                  # rebuild + smoke test the laptop stack
```
`TSP_DEPLOY_HOST` can replace `--host`. `make deploy-staging` / `make deploy-production`
are shortcuts.

What `apply.sh deploy` does on the host, in order:
1. Takes a lock, so two deploys can't run at once.
2. Decrypts `secrets.enc.yaml` with the host's age key straight into 0400 files on tmpfs.
3. Starts Postgres if needed. **Backs up the database** (`pg_dump`) to
   `/var/backups/talksasa-pay/<env>/` (keeps 30).
4. Runs migrations. If they fail, it stops; the running version is untouched.
5. Starts the new api, admin and pay containers. It waits for their health
   checks, then checks the public edge through Caddy.
6. If anything is unhealthy it **restores the previous version** automatically
   and prints the failed containers' logs.
7. Records the deploy in `/var/log/talksasa-pay/deploys.log` and keeps the last 5 releases.

Guards:
- **Production:**
  - only from a clean `main` that equals `origin/main`;
  - `make verify` always runs, and `--skip-verify` / `--allow-dirty` are refused;
  - refuses the sandbox paybill 174379 or the placeholder 000000.
- **Staging:** `--allow-dirty`, `--skip-verify` and `--yes` are allowed.

**Migrations are never rolled back.** Write them expand-then-contract: the
previous release must still work on the new schema. After a bad migration,
restore from the pre-deploy dump (below).

**Downtime:** the api, admin and pay containers are replaced in place, so
expect roughly 10–20 s of errors per deploy. Postgres and Caddy are not
restarted. Deploy outside peak payment hours. Once Daraja callbacks are live
(Phase 2), Safaricom does not guarantee callback retries, so reconciliation
(Phase 5) covers anything missed during the swap.

## One-time host setup
1. Ubuntu 24.04 LTS with Docker Engine and the compose plugin, `sops` (from
   the getsops GitHub release, checksum verified), `openssl`, `curl`.
2. A `deploy` user with your SSH key and passwordless sudo. Add the host key to
   your `~/.ssh/known_hosts` first: the script refuses unknown hosts.
3. Firewall: allow 22 (from the office only), 80 and 443. Nothing else.
4. DNS for the three domains points at the host.
5. Age key (private key never leaves the host, plus one offline backup):
   ```sh
   sudo mkdir -p /etc/talksasa-pay && sudo age-keygen -o /etc/talksasa-pay/age.key
   sudo chmod 0400 /etc/talksasa-pay/age.key      # prints the public key "age1..."
   ```
6. Settings: copy `deploy/env.example` to `/etc/talksasa-pay/<env>.env` and
   fill it in: domains, real paybill, ACME email, admin allow-list.
7. Secrets, on your workstation:
   ```sh
   scripts/deploy.sh init-secrets production --age age1...   # writes secrets/production.enc.yaml + .sops.yaml
   git add .sops.yaml secrets/production.enc.yaml && git commit
   ```
   This generates database passwords, the master keyring and the API key
   pepper. They exist only encrypted. To change one later: `sops secrets/production.enc.yaml`.
8. First deploy, then create the first admin on the host:
   ```sh
   sudo docker compose -p talksasa-pay-production exec api /talksasa-pay admin user invite \
     --email you@talksasa.co.ke --name "Your Name" --role admin --actor "Your Name"
   ```
   Open the printed link from the office network: the admin site only answers
   inside `TSP_ADMIN_ALLOW_CIDRS`.

After a reboot, the `talksasa-pay-secrets@<env>` systemd unit (installed by
the first deploy) decrypts the secrets again before Docker starts the containers.

## Restoring a backup
```sh
sudo docker compose -p talksasa-pay-production stop api admin pay
sudo docker compose -p talksasa-pay-production exec -T -u postgres postgres \
  pg_restore --clean --if-exists -d talksasa_pay < /var/backups/talksasa-pay/production/<file>.dump
scripts/deploy.sh rollback production --host ...     # or redeploy the matching version
```
Payments that arrived after the dump was taken are not in it. Reconcile them
against the M-Pesa statement before reopening.

## What is exposed
| Domain | Serves |
|---|---|
| `TSP_PAY_DOMAIN` | customer pay pages |
| `TSP_ADMIN_DOMAIN` | staff panel, **only** from `TSP_ADMIN_ALLOW_CIDRS`, 404 for everyone else |
| `TSP_API_DOMAIN` | `/v1/*` product API, `/cb/*` Safaricom callbacks, `/healthz`; everything else is 404 |

- The admin API (:8089), Postgres and the Next.js servers are not published.
- Metrics are on the host's `127.0.0.1:9464` only.
- Postgres accepts network logins only over TLS with SCRAM, and only for the
  app and migration roles.
