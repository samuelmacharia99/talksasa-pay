# Runbook: secrets

## Principles
- Secrets never live in code, images, git (except SOPS-encrypted), logs, API
  responses or plain-text database columns.
- In staging and production the service **refuses** to read secrets from plain
  environment variables. Every secret is a file (`TSP_*_FILE`) mounted
  read-only from tmpfs, mode 0400/0600. Wider permissions are rejected at
  startup.
- `config.Secret` redacts itself in every format path (`fmt`, `slog`, JSON),
  and the logger additionally masks sensitive-looking keys and phone numbers.

## Inventory
| Secret | File variable | Used by | Rotation |
|---|---|---|---|
| App DB URL (role `talksasa_app`, no DELETE) | `TSP_DATABASE_URL_FILE` | api, worker, admin | change role password, redeploy |
| Migration DB URL (role `talksasa_owner`) | `TSP_MIGRATE_DATABASE_URL_FILE` | migrate only | as above |
| Master keyring (AES-256-GCM, versioned) | `TSP_MASTER_KEYS_FILE` | encrypting product webhook secrets (later: other DB-stored secrets) | add a new version line, deploy, re-encrypt, remove old line |
| API key pepper | `TSP_API_KEY_PEPPER_FILE` | hashing product API keys | rotating invalidates all keys: plan a key re-issue |
| Daraja consumer key/secret, passkey, B2C initiator credential | added in Phases 2 and 4 | worker/api | per Safaricom process |

## Storage: SOPS + age
1. Each environment has an age key pair. The private key lives only on the
   deploy host (`/etc/talksasa-pay/age.key`, root 0400) and in the offline
   backup. The public key is in `.sops.yaml`.
2. Secrets are kept in `secrets/<env>.enc.yaml`, encrypted with
   `sops --encrypt`. Only the encrypted file is committed.
3. At deploy, `sops --decrypt` writes each value to
   `/run/talksasa-pay/secrets/<name>` (tmpfs, mode 0400, owned by the
   container uid). `deploy/compose.prod.yml` (written with the deployment work) mounts those as
   Docker secrets. Nothing
   is written to persistent disk.

## Generating keys
```sh
talksasa-pay keygen --version 1                  # master keyring line "1:<base64>"
head -c 48 /dev/urandom | base64                 # API key pepper (>= 32 chars)
```

## Rotating the master key
1. `talksasa-pay keygen --version N+1` and append the line to the keyring.
   Keep the old line.
2. Deploy. New encryptions use version N+1; old rows still decrypt.
3. Re-encrypt old rows (rotate each product's webhook secret with
   `admin product rotate-webhook-secret`, or the bulk re-encrypt command due
   in Phase 7).
4. When no row uses version N, remove its line and deploy.

## Local development
`make dev-secrets` creates throwaway values in `deploy/dev-secrets/`
(gitignored). Never copy them anywhere else.
