# Runbook: Settings (M-Pesa paybill)

The admin panel's **Settings** page (Admin → Settings) configures the one
M-Pesa paybill the whole gateway collects with. Everyone can open it;
only the **admin** role can change anything (`settings.manage`).

## What is on the page

- **Paybill:** environment (sandbox or production), paybill number,
  optional STK short code (empty = the paybill), callback base URL, and the
  Daraja consumer key, consumer secret and passkey.
- **Connection:** **Test credentials** asks Daraja for an access token with
  the settings in effect and shows the result and time. The token is
  neither kept nor shown.
- **Callback URLs:** the confirmation URL with the callback token masked,
  the last registration (who and when; flagged if it was for an older
  paybill or URL), and **Register URLs with Safaricom** (C2B, response
  type Completed).

## Rules

- **Write-only credentials.** They are encrypted with the master keyring
  (AES-256-GCM, as for app webhook secrets) in the `mpesa_settings` table.
  The page shows only "Saved ••••1234" (last 4 characters, for values of
  12 characters or more) or "Not set". Leave a credential field empty to
  keep the saved value.
- **Audited.** Saving writes `mpesa_settings.updated` with which fields
  changed (never their values); tests write `mpesa_settings.tested`;
  registrations write `mpesa.paybill_urls_registered` with the token
  masked.
- **Production needs a typed confirmation**: type `production` to save
  production settings (or to change away from them) and to register URLs
  for a production paybill. Production is allowed only on staging and
  production deployments, needs `TSP_MPESA_CALLBACK_CIDRS`, and a
  production deployment accepts only production.
- **Database first, SOPS as fallback.** Until something is saved here the
  api and worker use the `TSP_MPESA_*` configuration (the page says
  "Deployment configuration"). Once saved, the page's settings win. The
  callback token, callback networks and timeouts always come from the
  deployment configuration.
- **No restart.** Each api and worker process re-reads the settings version
  at most every 5 seconds and rebuilds its Daraja client when it changed
  (the process that saved applies it at once). The pay page's paybill
  number, whether prompts are offered, and the "C2B for another business
  number" alarm all follow the settings in effect.

## Common tasks

- **First setup:** fill everything, **Save settings**, **Test
  credentials**, then **Register URLs with Safaricom**. Then follow the
  smoke test in [mpesa.md](mpesa.md).
- **Rotate a credential:** paste only the new value, save, test.
- **Move the API domain:** change the callback base URL, save, register
  again (the old registration is shown as for an older URL). In production
  re-registration goes through Safaricom; see mpesa.md.
- **"TSP_MPESA_CALLBACK_TOKEN is not set":** the operator must add
  `mpesa_callback_token` to the SOPS bundle and deploy; M-Pesa stays off
  until then.
- **Master key rotation:** the settings row is sealed with one key version
  (`key_version`); saving the page re-seals all three credentials with the
  active key. Keep old key versions in the keyring until the row has been
  saved again.

## Command line

`talksasa-pay admin mpesa status` shows the settings in effect and where
they come from; `talksasa-pay admin mpesa register-urls --actor NAME
[--confirm production]` does what the button does.
