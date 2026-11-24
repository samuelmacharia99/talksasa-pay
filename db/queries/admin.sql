-- Staff accounts, invitations, sessions and recovery codes.

-- name: CreateAdminUser :one
INSERT INTO admin_users (id, email, name, role, created_by)
VALUES (@id, @email, @name, @role, @created_by)
RETURNING *;

-- name: GetAdminUser :one
SELECT * FROM admin_users WHERE id = @id;

-- name: GetAdminUserByEmail :one
SELECT * FROM admin_users WHERE email = @email;

-- name: LockAdminUser :one
SELECT * FROM admin_users WHERE id = @id FOR NO KEY UPDATE;

-- name: ListAdminUsers :many
SELECT * FROM admin_users ORDER BY status, lower(name), id;

-- name: CountActiveAdmins :one
SELECT count(*)::bigint FROM admin_users WHERE role = 'admin' AND status = 'active';

-- name: ActivateAdminUser :one
UPDATE admin_users
   SET status = 'active', password_hash = @password_hash,
       totp_ciphertext = @totp_ciphertext, totp_key_version = @totp_key_version, totp_last_step = @totp_last_step,
       password_changed_at = now(), failed_attempts = 0, locked_until = NULL
 WHERE id = @id AND status = 'invited'
RETURNING *;

-- name: ResetAdminUser :one
-- Back to `invited`: wipes password and TOTP so the old credentials stop working.
UPDATE admin_users
   SET status = 'invited', password_hash = NULL, totp_ciphertext = NULL, totp_key_version = NULL,
       totp_last_step = NULL, failed_attempts = 0, locked_until = NULL, disabled_at = NULL, disabled_by = NULL
 WHERE id = @id
RETURNING *;

-- name: DisableAdminUser :one
UPDATE admin_users
   SET status = 'disabled', disabled_at = now(), disabled_by = @disabled_by
 WHERE id = @id AND status <> 'disabled'
RETURNING *;

-- name: SetAdminUserRole :one
UPDATE admin_users SET role = @role WHERE id = @id RETURNING *;

-- name: RecordAdminLoginFailure :one
-- Counts a failed password or 2FA attempt. From the 5th consecutive failure
-- the account locks for 1 min, doubling per further failure, capped at 1 h.
UPDATE admin_users
   SET failed_attempts = failed_attempts + 1,
       locked_until = CASE WHEN failed_attempts + 1 >= 5
                           THEN sqlc.arg(now)::timestamptz + least(interval '1 minute' * power(2, least(failed_attempts + 1 - 5, 10)), interval '1 hour')
                      END
 WHERE id = @id
RETURNING failed_attempts, locked_until;

-- name: RecordAdminLoginSuccess :exec
UPDATE admin_users SET failed_attempts = 0, locked_until = NULL, last_login_at = sqlc.arg(now)::timestamptz WHERE id = @id;

-- name: ClaimTOTPStep :execrows
-- Atomically records the TOTP time step just used. 0 rows = the step (or a
-- later one) was already used: a replayed code.
UPDATE admin_users
   SET totp_last_step = @step
 WHERE id = @id AND (totp_last_step IS NULL OR totp_last_step < @step);

-- name: UpdateAdminPassword :exec
UPDATE admin_users SET password_hash = @password_hash, password_changed_at = now() WHERE id = @id;

-- name: CreateAdminInvitation :one
INSERT INTO admin_invitations (id, user_id, token_hash, created_by, expires_at)
VALUES (@id, @user_id, @token_hash, @created_by, @expires_at)
RETURNING *;

-- name: RevokeAdminInvitations :exec
UPDATE admin_invitations SET revoked_at = now()
 WHERE user_id = @user_id AND used_at IS NULL AND revoked_at IS NULL;

-- name: GetAdminInvitationForUpdate :one
SELECT * FROM admin_invitations WHERE token_hash = @token_hash FOR NO KEY UPDATE;

-- name: MarkAdminInvitationUsed :exec
UPDATE admin_invitations SET used_at = now() WHERE id = @id AND used_at IS NULL AND revoked_at IS NULL;

-- name: CreateAdminSession :one
INSERT INTO admin_sessions (id, user_id, token_hash, stage, created_at, last_seen_at, expires_at, ip, user_agent)
VALUES (@id, @user_id, @token_hash, @stage, @now, @now, @expires_at, @ip, @user_agent)
RETURNING *;

-- name: GetAdminSessionByHash :one
SELECT s.id, s.user_id, s.stage, s.created_at, s.last_seen_at, s.expires_at, s.revoked_at,
       u.email, u.name, u.role, u.status AS user_status
  FROM admin_sessions s
  JOIN admin_users u ON u.id = s.user_id
 WHERE s.token_hash = @token_hash;

-- name: TouchAdminSession :exec
-- Throttled: idle-timeout precision of 30 s is plenty and saves a write per request.
-- Times come from the Go clock so every session decision uses one clock.
UPDATE admin_sessions SET last_seen_at = @now
 WHERE id = @id AND revoked_at IS NULL AND last_seen_at < sqlc.arg(now)::timestamptz - interval '30 seconds';

-- name: RevokeAdminSession :exec
UPDATE admin_sessions SET revoked_at = now(), revoke_reason = @reason WHERE id = @id AND revoked_at IS NULL;

-- name: RevokeAdminUserSessions :exec
UPDATE admin_sessions SET revoked_at = now(), revoke_reason = @reason WHERE user_id = @user_id AND revoked_at IS NULL;

-- name: InsertAdminRecoveryCode :exec
INSERT INTO admin_recovery_codes (id, user_id, code_hash) VALUES (@id, @user_id, @code_hash);

-- name: RevokeAdminRecoveryCodes :exec
UPDATE admin_recovery_codes SET revoked_at = now()
 WHERE user_id = @user_id AND used_at IS NULL AND revoked_at IS NULL;

-- name: UseAdminRecoveryCode :execrows
UPDATE admin_recovery_codes SET used_at = now()
 WHERE user_id = @user_id AND code_hash = @code_hash AND used_at IS NULL AND revoked_at IS NULL;

-- name: CountAdminRecoveryCodes :one
SELECT count(*)::bigint FROM admin_recovery_codes WHERE user_id = @user_id AND used_at IS NULL AND revoked_at IS NULL;
