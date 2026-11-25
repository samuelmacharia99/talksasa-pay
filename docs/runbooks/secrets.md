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
| Master keyring (AES-256-GCM, versioned) | `TSP_MASTER_KEYS_FILE` | encrypting app webhook secrets (later: other DB-stored secrets) | add a new version line, deploy, re-encrypt, remove old line |
| API key pepper | `TSP_API_KEY_PEPPER_FILE` | hashing app API keys | rotating invalidates all keys: plan a key re-issue |
| Daraja consumer key (bundle key `mpesa_consumer_key`) | `TSP_MPESA_CONSUMER_KEY_FILE` | api, worker (OAuth for STK push/query, C2B URL registration) | regenerate on the Daraja portal app, update the bundle, redeploy |
| Daraja consumer secret (`mpesa_consumer_secret`) | `TSP_MPESA_CONSUMER_SECRET_FILE` | api, worker | as above (rotate together with the key) |
| Lipa na M-Pesa Online passkey (`mpesa_passkey`) | `TSP_MPESA_PASSKEY_FILE` | api, worker (STK password) | issued by Safaricom for the shortcode; request a new one, update the bundle, redeploy |
| M-Pesa callback token (`mpesa_callback_token`, >= 32 URL-safe chars, e.g. `head -c 32 /dev/urandom \| base64 \| tr '+/' '-_' \| tr -d '='`) | `TSP_MPESA_CALLBACK_TOKEN_FILE` | api (checks `/cb/mpesa/{token}/...`), worker/CLI (builds callback URLs) | new value, redeploy, then re-register C2B URLs (`admin mpesa register-urls`); STK pushes in flight keep the old URL, so rotate when none are pending |
| B2C initiator credential | added in Phase 4 | worker | per Safaricom process |

The four `mpesa_*` keys are only required when `TSP_MPESA_ENV` is set in
`/etc/talksasa-pay/<env>.env`; `apply.sh` refuses to deploy if one is
missing then. With M-Pesa off they may be absent from the bundle (apply.sh
mounts placeholder files the service never reads). Add them with
`sops secrets/<env>.enc.yaml` (never paste values into chat or tickets).

## Storage: SOPS + age
1. Each environment has an age key pair. The private key lives only on the
   deploy host (`/etc/talksasa-pay/age.key`, root 0400) and in the offline
   backup. The public key is in `.sops.yaml`.
2. Secrets are kept in `secrets/<env>.enc.yaml`, encrypted with
   `sops --encrypt`. Only the encrypted file is committed.
3. At deploy, `sops --decrypt` writes each value to
   `/run/talksasa-pay/secrets/<name>` (tmpfs, mode 0400, owned by the
   container uid). `deploy/compose.prod.yml` mounts those as Docker secrets. Nothing is written
   to persistent disk. `scripts/deploy.sh init-secrets <env> --age age1...`
   creates a new bundle with fresh random values ([deploy runbook](deploy.md)).

## Generating keys
```sh
talksasa-pay keygen --version 1                  # master keyring line "1:<base64>"
head -c 48 /dev/urandom | base64                 # API key pepper (>= 32 chars)
```

## Rotating the master key
1. `talksasa-pay keygen --version N+1` and append the line to the keyring.
   Keep the old line.
2. Deploy. New encryptions use version N+1; old rows still decrypt.
3. Re-encrypt old rows (rotate each app's webhook secret with
   `admin app rotate-webhook-secret`, or the bulk re-encrypt command due
   in Phase 7).
4. When no row uses version N, remove its line and deploy.

## Local development
`make dev-secrets` creates throwaway values in `deploy/dev-secrets/`
(gitignored). Never copy them anywhere else.
