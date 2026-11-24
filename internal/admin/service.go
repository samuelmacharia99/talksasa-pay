// Package admin manages Talksasa staff accounts for the admin panel:
// invitations, password + mandatory TOTP sign-in, server-side sessions and
// role-based permissions.
//
// Secrets handled here never leave the package in loggable form: passwords
// are argon2id-hashed, TOTP secrets are AES-GCM encrypted with the master
// keyring, and session / invitation / recovery tokens are stored only as
// digests. The only plaintext tokens returned are those the caller must hand
// to the user exactly once (setup link, session cookie, recovery codes).
package admin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

var (
	// ErrInvalidCredentials is returned for every sign-in failure (unknown
	// email, wrong password, disabled or locked account) so the response
	// never reveals which accounts exist.
	ErrInvalidCredentials = errors.New("admin: sign-in failed")
	ErrInvalidCode        = errors.New("admin: invalid or already used authentication code")
	ErrSessionInvalid     = errors.New("admin: session invalid or expired")
	ErrInviteInvalid      = errors.New("admin: setup link invalid, used or expired")
	ErrEnrolmentInvalid   = errors.New("admin: two-factor enrolment data invalid; reload the setup page")
	ErrEmailTaken         = errors.New("admin: a staff account with this email already exists")
	ErrInvalidInput       = errors.New("admin: invalid input")
	ErrNotFound           = errors.New("admin: staff account not found")
	ErrSelfAction         = errors.New("admin: you cannot do this to your own account")
	ErrLastAdmin          = errors.New("admin: this would leave no active admin")
	ErrAlreadyDisabled    = errors.New("admin: account already disabled")
)

const (
	recoveryCodeCount = 10
	tokenBytes        = 32 // session and invitation tokens: 256 bits
)

var (
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	// Tokens are lowercase unpadded base32 of 32 random bytes.
	tokenRe    = regexp.MustCompile(`^[a-z2-7]{52}$`)
	recoveryRe = regexp.MustCompile(`^[a-z2-7]{12}$`)
	digitsRe   = regexp.MustCompile(`^[0-9]{6}$`)
)

// Config holds session and invitation lifetimes.
type Config struct {
	IdleTimeout     time.Duration // full session ends after this long without use
	AbsoluteTimeout time.Duration // full session ends this long after sign-in, regardless of use
	MFATimeout      time.Duration // time allowed between password and 2FA steps
	InviteTTL       time.Duration // setup link lifetime
	SetupBaseURL    string        // e.g. https://admin.example.com/setup/ ; the token is appended
	HashConcurrency int           // max parallel argon2 computations
}

// DefaultConfig returns the production defaults.
func DefaultConfig() Config {
	return Config{
		IdleTimeout: 15 * time.Minute, AbsoluteTimeout: 8 * time.Hour, MFATimeout: 5 * time.Minute,
		InviteTTL: 24 * time.Hour, SetupBaseURL: "http://127.0.0.1:3001/setup/", HashConcurrency: 4,
	}
}

// Options configures the Service.
type Options struct {
	Pool    *db.Pool
	Keyring *crypto.Keyring
	Pepper  []byte // server-side pepper for recovery-code HMACs
	Config  Config
	Logger  *slog.Logger
	// Now overrides the clock (tests). Every session, lockout and invitation
	// time decision uses this one clock.
	Now func() time.Time
}

// Service implements staff account management and authentication.
type Service struct {
	pool   *db.Pool
	keys   *crypto.Keyring
	pepper []byte
	cfg    Config
	logger *slog.Logger
	hash   *hasher
	now    func() time.Time
}

// NewService builds the admin service.
func NewService(o Options) *Service {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		pool: o.Pool, keys: o.Keyring, pepper: o.Pepper, cfg: o.Config, logger: o.Logger.With(slog.String("component", "admin")),
		hash: newHasher(defaultArgon, o.Config.HashConcurrency), now: func() time.Time { return now().UTC().Truncate(time.Microsecond) },
	}
}

// User is the non-secret view of a staff account.
type User struct {
	ID          uuid.UUID
	Email       string
	Name        string
	Role        store.AdminRole
	Status      store.AdminUserStatus
	LastLoginAt *time.Time
	CreatedBy   string
	CreatedAt   time.Time
	DisabledAt  *time.Time
}

func userFromRow(u store.AdminUser) User {
	return User{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role, Status: u.Status, LastLoginAt: u.LastLoginAt,
		CreatedBy: u.CreatedBy, CreatedAt: u.CreatedAt, DisabledAt: u.DisabledAt}
}

// Principal is a signed-in staff member, resolved from a session token.
type Principal struct {
	UserID        uuid.UUID
	Email         string
	Name          string
	Role          store.AdminRole
	SessionID     uuid.UUID
	ExpiresAt     time.Time // absolute expiry
	IdleExpiresAt time.Time // expiry if not used again
}

// Actor returns the audit actor for this principal.
func (p Principal) Actor() audit.Actor { return audit.Admin(p.UserID) }

// Can reports whether the principal's role grants perm.
func (p Principal) Can(perm Permission) bool { return Allows(p.Role, perm) }

// Client describes where a request came from, for sessions and the audit log.
type Client struct {
	IP        *netip.Addr
	UserAgent string
}

func (c Client) ipString() string {
	if c.IP == nil {
		return ""
	}
	return c.IP.String()
}

func (c Client) ua() *string {
	if c.UserAgent == "" {
		return nil
	}
	ua := c.UserAgent
	if len(ua) > 300 {
		ua = ua[:300]
	}
	return &ua
}

func digest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// ---------------------------------------------------------------------------
// Invitations
// ---------------------------------------------------------------------------

// Invitation is returned once when a setup link is issued.
type Invitation struct {
	User      User
	Token     string
	URL       string
	ExpiresAt time.Time
}

// Invite creates a staff account in `invited` state and a one-time setup
// link. No password ever passes through the inviter.
func (s *Service) Invite(ctx context.Context, actor audit.Actor, email, name string, role store.AdminRole) (Invitation, error) {
	email = normalizeEmail(email)
	name = strings.TrimSpace(name)
	if len(email) > 254 || !emailRe.MatchString(email) || name == "" || len(name) > 100 || !role.Valid() {
		return Invitation{}, fmt.Errorf("%w: a valid email, a name (1-100 characters) and a role are required", ErrInvalidInput)
	}
	var out Invitation
	err := db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		u, err := q.CreateAdminUser(ctx, store.CreateAdminUserParams{ID: id, Email: email, Name: name, Role: role, CreatedBy: actor.String()})
		if err != nil {
			if db.IsUniqueViolation(err, "admin_users_email_key") {
				return ErrEmailTaken
			}
			return err
		}
		token, exp, err := s.issueInvite(ctx, q, u.ID, actor.String())
		if err != nil {
			return err
		}
		out = Invitation{User: userFromRow(u), Token: token, URL: s.cfg.SetupBaseURL + token, ExpiresAt: exp}
		return audit.Record(ctx, q, audit.Entry{Actor: actor, Action: "admin_user.invited", EntityType: "admin_user",
			EntityID: u.ID.String(), Details: map[string]any{"email": email, "role": string(role)}})
	})
	if err != nil {
		return Invitation{}, err
	}
	s.logger.InfoContext(ctx, "staff user invited", slog.String("admin_user_id", out.User.ID.String()), slog.String("role", string(role)))
	return out, nil
}

func (s *Service) issueInvite(ctx context.Context, q *store.Queries, userID uuid.UUID, createdBy string) (string, time.Time, error) {
	token, err := crypto.RandomToken(tokenBytes)
	if err != nil {
		return "", time.Time{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", time.Time{}, err
	}
	if err := q.RevokeAdminInvitations(ctx, userID); err != nil {
		return "", time.Time{}, err
	}
	exp := s.now().Add(s.cfg.InviteTTL).UTC().Truncate(time.Microsecond)
	if _, err := q.CreateAdminInvitation(ctx, store.CreateAdminInvitationParams{
		ID: id, UserID: userID, TokenHash: digest(token), CreatedBy: createdBy, ExpiresAt: exp,
	}); err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
}

// Reset sends a staff member back to `invited`: password, TOTP and recovery
// codes are wiped, every session is signed out, and a new setup link is
// issued. Used for lost 2FA devices and re-onboarding disabled staff.
// byUser is the acting staff member (nil for the CLI); nobody can reset
// their own account through the panel.
func (s *Service) Reset(ctx context.Context, actor audit.Actor, byUser *uuid.UUID, userID uuid.UUID) (Invitation, error) {
	if byUser != nil && *byUser == userID {
		return Invitation{}, ErrSelfAction
	}
	var out Invitation
	err := db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		if _, err := q.LockAdminUser(ctx, userID); err != nil {
			if db.IsNotFound(err) {
				return ErrNotFound
			}
			return err
		}
		u, err := q.ResetAdminUser(ctx, userID)
		if err != nil {
			return err
		}
		if err := q.RevokeAdminUserSessions(ctx, store.RevokeAdminUserSessionsParams{UserID: userID, Reason: strPtr("account_reset")}); err != nil {
			return err
		}
		if err := q.RevokeAdminRecoveryCodes(ctx, userID); err != nil {
			return err
		}
		token, exp, err := s.issueInvite(ctx, q, userID, actor.String())
		if err != nil {
			return err
		}
		out = Invitation{User: userFromRow(u), Token: token, URL: s.cfg.SetupBaseURL + token, ExpiresAt: exp}
		return audit.Record(ctx, q, audit.Entry{Actor: actor, Action: "admin_user.reset", EntityType: "admin_user", EntityID: userID.String()})
	})
	if err != nil {
		return Invitation{}, err
	}
	s.logger.InfoContext(ctx, "staff user reset", slog.String("admin_user_id", userID.String()))
	return out, nil
}

// ---------------------------------------------------------------------------
// Setup (accepting an invitation)
// ---------------------------------------------------------------------------

// SetupInfo is shown on the setup page. Enrolment is an opaque, encrypted
// blob carrying the freshly generated TOTP secret; the page posts it back
// with the first code. Secret and OTPAuthURI are shown to the enrolling user
// only (QR code and manual-entry key).
type SetupInfo struct {
	Email      string
	Name       string
	ExpiresAt  time.Time
	Enrolment  string
	Secret     string
	OTPAuthURI string
}

// validInvite resolves a setup token to its invitation and invited user.
func (s *Service) validInvite(ctx context.Context, q *store.Queries, token string) (store.AdminInvitation, store.AdminUser, error) {
	if !tokenRe.MatchString(token) {
		return store.AdminInvitation{}, store.AdminUser{}, ErrInviteInvalid
	}
	inv, err := q.GetAdminInvitationForUpdate(ctx, digest(token))
	if err != nil {
		if db.IsNotFound(err) {
			return inv, store.AdminUser{}, ErrInviteInvalid
		}
		return inv, store.AdminUser{}, err
	}
	if inv.UsedAt != nil || inv.RevokedAt != nil || !s.now().Before(inv.ExpiresAt) {
		return inv, store.AdminUser{}, ErrInviteInvalid
	}
	u, err := q.LockAdminUser(ctx, inv.UserID)
	if err != nil {
		return inv, u, err
	}
	if u.Status != store.AdminUserStatusInvited {
		return inv, u, ErrInviteInvalid
	}
	return inv, u, nil
}

func enrolmentAAD(invitationID uuid.UUID) []byte {
	return []byte("admin_setup.totp:" + invitationID.String())
}

func totpAAD(userID uuid.UUID) []byte { return []byte("admin_users.totp:" + userID.String()) }

// BeginSetup validates a setup link and generates a TOTP secret to enrol.
func (s *Service) BeginSetup(ctx context.Context, token string) (SetupInfo, error) {
	var out SetupInfo
	err := db.WithTx(ctx, s.pool, "setup", func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		inv, u, err := s.validInvite(ctx, q, token)
		if err != nil {
			return err
		}
		secret, err := crypto.RandomBytes(totpSecretSize)
		if err != nil {
			return err
		}
		ct, ver, err := s.keys.Seal(secret, enrolmentAAD(inv.ID))
		if err != nil {
			return err
		}
		out = SetupInfo{
			Email: u.Email, Name: u.Name, ExpiresAt: inv.ExpiresAt,
			Enrolment:  strconv.Itoa(ver) + "." + base64.RawURLEncoding.EncodeToString(ct),
			Secret:     totpB32.EncodeToString(secret),
			OTPAuthURI: otpauthURI(secret, u.Email),
		}
		return nil
	})
	return out, err
}

func (s *Service) openEnrolment(enrolment string, invitationID uuid.UUID) ([]byte, error) {
	verStr, b64, ok := strings.Cut(enrolment, ".")
	if !ok || len(enrolment) > 512 {
		return nil, ErrEnrolmentInvalid
	}
	ver, err := strconv.Atoi(verStr)
	if err != nil {
		return nil, ErrEnrolmentInvalid
	}
	ct, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return nil, ErrEnrolmentInvalid
	}
	secret, err := s.keys.Open(ct, ver, enrolmentAAD(invitationID))
	if err != nil || len(secret) != totpSecretSize {
		return nil, ErrEnrolmentInvalid
	}
	return secret, nil
}

// CompleteSetup sets the password, enrols TOTP (proven by a valid code) and
// activates the account. It returns the recovery codes, to be shown once.
func (s *Service) CompleteSetup(ctx context.Context, token, enrolment, password, code string, client Client) ([]string, error) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	// Validate and do the slow work (argon2) before taking row locks.
	var inv store.AdminInvitation
	var user store.AdminUser
	if err := db.WithTx(ctx, s.pool, "setup", func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		inv, user, err = s.validInvite(ctx, q, token)
		return err
	}); err != nil {
		return nil, err
	}
	if err := CheckPasswordPolicy(password, user.Email, user.Name); err != nil {
		return nil, err
	}
	secret, err := s.openEnrolment(enrolment, inv.ID)
	if err != nil {
		return nil, err
	}
	step, ok := verifyTOTP(secret, code, s.now(), nil)
	if !ok || !digitsRe.MatchString(code) {
		return nil, ErrInvalidCode
	}
	phc, err := s.hash.Hash(ctx, password)
	if err != nil {
		return nil, err
	}
	ct, ver, err := s.keys.Seal(secret, totpAAD(user.ID))
	if err != nil {
		return nil, err
	}
	codes, hashes, err := s.newRecoveryCodes(user.ID)
	if err != nil {
		return nil, err
	}

	actor := audit.Admin(user.ID)
	err = db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		inv2, _, err := s.validInvite(ctx, q, token)
		if err != nil {
			return err
		}
		if inv2.ID != inv.ID {
			return ErrInviteInvalid
		}
		ver32 := int32(ver)
		if _, err := q.ActivateAdminUser(ctx, store.ActivateAdminUserParams{
			ID: user.ID, PasswordHash: &phc, TotpCiphertext: ct, TotpKeyVersion: &ver32, TotpLastStep: &step,
		}); err != nil {
			return err
		}
		if err := q.MarkAdminInvitationUsed(ctx, inv.ID); err != nil {
			return err
		}
		if err := s.storeRecoveryCodes(ctx, q, user.ID, hashes); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Actor: actor, Action: "admin_user.activated", EntityType: "admin_user",
			EntityID: user.ID.String(), Details: map[string]any{"ip": client.ipString()}})
	})
	if err != nil {
		return nil, err
	}
	s.logger.InfoContext(ctx, "staff user activated", slog.String("admin_user_id", user.ID.String()))
	return codes, nil
}

// ---------------------------------------------------------------------------
// Recovery codes
// ---------------------------------------------------------------------------

func (s *Service) recoveryHash(userID uuid.UUID, code string) []byte {
	return crypto.HMACSHA256(s.pepper, []byte("tsp-recovery:"+userID.String()+":"+code))
}

func normalizeRecovery(code string) string {
	return strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
}

func (s *Service) newRecoveryCodes(userID uuid.UUID) (codes []string, hashes [][]byte, err error) {
	for range recoveryCodeCount {
		raw, err := crypto.RandomToken(7) // 56 bits -> 12 base32 characters
		if err != nil {
			return nil, nil, err
		}
		raw = raw[:12]
		codes = append(codes, raw[:6]+"-"+raw[6:])
		hashes = append(hashes, s.recoveryHash(userID, raw))
	}
	return codes, hashes, nil
}

func (s *Service) storeRecoveryCodes(ctx context.Context, q *store.Queries, userID uuid.UUID, hashes [][]byte) error {
	if err := q.RevokeAdminRecoveryCodes(ctx, userID); err != nil {
		return err
	}
	for _, h := range hashes {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if err := q.InsertAdminRecoveryCode(ctx, store.InsertAdminRecoveryCodeParams{ID: id, UserID: userID, CodeHash: h}); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sign-in
// ---------------------------------------------------------------------------

// Challenge is the short-lived pre-2FA session returned by a correct
// password. It can only be exchanged for a full session via VerifyMFA.
type Challenge struct {
	Token     string
	ExpiresAt time.Time
}

// Session is a full session; Token goes in the httpOnly session cookie.
type Session struct {
	Token     string
	ExpiresAt time.Time
	Principal Principal
}

// Login checks email and password. Every failure returns
// ErrInvalidCredentials after the same amount of argon2 work.
func (s *Service) Login(ctx context.Context, email, password string, client Client) (Challenge, error) {
	email = normalizeEmail(email)
	if len(password) > maxPasswordLen*4 || len(email) > 254 {
		return Challenge{}, ErrInvalidCredentials
	}
	q := store.New(s.pool)
	u, err := q.GetAdminUserByEmail(ctx, email)
	if err != nil {
		if !db.IsNotFound(err) {
			return Challenge{}, err
		}
		s.hash.VerifyDummy(ctx, password)
		s.logger.WarnContext(ctx, "staff sign-in failed", slog.String("reason", "unknown_email"), slog.String("ip", client.ipString()))
		return Challenge{}, ErrInvalidCredentials
	}
	if u.Status != store.AdminUserStatusActive || u.PasswordHash == nil {
		s.hash.VerifyDummy(ctx, password)
		s.logger.WarnContext(ctx, "staff sign-in failed", slog.String("reason", "account_not_active"),
			slog.String("admin_user_id", u.ID.String()), slog.String("ip", client.ipString()))
		return Challenge{}, ErrInvalidCredentials
	}
	if u.LockedUntil != nil && s.now().Before(*u.LockedUntil) {
		s.hash.VerifyDummy(ctx, password)
		s.logger.WarnContext(ctx, "staff sign-in failed", slog.String("reason", "locked"),
			slog.String("admin_user_id", u.ID.String()), slog.String("ip", client.ipString()))
		return Challenge{}, ErrInvalidCredentials
	}
	ok, rehash, err := s.hash.Verify(ctx, password, *u.PasswordHash)
	if err != nil {
		return Challenge{}, err
	}
	if !ok {
		s.recordFailure(ctx, q, u.ID, "wrong_password", client)
		return Challenge{}, ErrInvalidCredentials
	}
	if rehash {
		if phc, err := s.hash.Hash(ctx, password); err == nil {
			if err := q.UpdateAdminPassword(ctx, store.UpdateAdminPasswordParams{ID: u.ID, PasswordHash: &phc}); err != nil {
				s.logger.WarnContext(ctx, "password rehash failed", slog.String("admin_user_id", u.ID.String()), slog.String("error", err.Error()))
			}
		}
	}
	token, exp, err := s.createSession(ctx, q, u.ID, store.AdminSessionStageMfa, s.cfg.MFATimeout, client)
	if err != nil {
		return Challenge{}, err
	}
	return Challenge{Token: token, ExpiresAt: exp}, nil
}

func (s *Service) recordFailure(ctx context.Context, q *store.Queries, userID uuid.UUID, reason string, client Client) {
	row, err := q.RecordAdminLoginFailure(ctx, store.RecordAdminLoginFailureParams{ID: userID, Now: s.now()})
	if err != nil {
		s.logger.ErrorContext(ctx, "recording sign-in failure", slog.String("admin_user_id", userID.String()), slog.String("error", err.Error()))
		return
	}
	attrs := []any{slog.String("reason", reason), slog.String("admin_user_id", userID.String()),
		slog.Int("failed_attempts", int(row.FailedAttempts)), slog.String("ip", client.ipString())}
	if row.LockedUntil != nil {
		attrs = append(attrs, slog.Time("locked_until", *row.LockedUntil))
	}
	s.logger.WarnContext(ctx, "staff sign-in failed", attrs...)
}

func (s *Service) createSession(ctx context.Context, q *store.Queries, userID uuid.UUID, stage store.AdminSessionStage, ttl time.Duration, client Client) (string, time.Time, error) {
	token, err := crypto.RandomToken(tokenBytes)
	if err != nil {
		return "", time.Time{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", time.Time{}, err
	}
	exp := s.now().Add(ttl).UTC().Truncate(time.Microsecond)
	if _, err := q.CreateAdminSession(ctx, store.CreateAdminSessionParams{
		ID: id, UserID: userID, TokenHash: digest(token), Stage: stage, Now: s.now(), ExpiresAt: exp, Ip: client.IP, UserAgent: client.ua(),
	}); err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
}

// VerifyMFA exchanges a password challenge plus a TOTP code (or a one-time
// recovery code) for a full session with a new token.
func (s *Service) VerifyMFA(ctx context.Context, challenge, code string, client Client) (Session, error) {
	if !tokenRe.MatchString(challenge) {
		return Session{}, ErrSessionInvalid
	}
	q := store.New(s.pool)
	sess, err := q.GetAdminSessionByHash(ctx, digest(challenge))
	if err != nil {
		if db.IsNotFound(err) {
			return Session{}, ErrSessionInvalid
		}
		return Session{}, err
	}
	if sess.Stage != store.AdminSessionStageMfa || sess.RevokedAt != nil || !s.now().Before(sess.ExpiresAt) ||
		sess.UserStatus != store.AdminUserStatusActive {
		return Session{}, ErrSessionInvalid
	}
	u, err := q.GetAdminUser(ctx, sess.UserID)
	if err != nil {
		return Session{}, err
	}
	if u.LockedUntil != nil && s.now().Before(*u.LockedUntil) {
		_ = q.RevokeAdminSession(ctx, store.RevokeAdminSessionParams{ID: sess.ID, Reason: strPtr("locked")})
		return Session{}, ErrSessionInvalid
	}

	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	usedRecovery := false
	verified := false
	switch {
	case digitsRe.MatchString(code):
		if u.TotpCiphertext == nil || u.TotpKeyVersion == nil {
			return Session{}, ErrSessionInvalid
		}
		secret, err := s.keys.Open(u.TotpCiphertext, int(*u.TotpKeyVersion), totpAAD(u.ID))
		if err != nil {
			return Session{}, fmt.Errorf("admin: decrypt TOTP secret: %w", err)
		}
		if step, ok := verifyTOTP(secret, code, s.now(), u.TotpLastStep); ok {
			n, err := q.ClaimTOTPStep(ctx, store.ClaimTOTPStepParams{ID: u.ID, Step: &step})
			if err != nil {
				return Session{}, err
			}
			verified = n == 1
		}
	case recoveryRe.MatchString(normalizeRecovery(code)):
		n, err := q.UseAdminRecoveryCode(ctx, store.UseAdminRecoveryCodeParams{UserID: u.ID, CodeHash: s.recoveryHash(u.ID, normalizeRecovery(code))})
		if err != nil {
			return Session{}, err
		}
		verified, usedRecovery = n == 1, n == 1
	}
	if !verified {
		s.recordFailure(ctx, q, u.ID, "wrong_2fa_code", client)
		return Session{}, ErrInvalidCode
	}

	var out Session
	actor := audit.Admin(u.ID)
	err = db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		if err := q.RevokeAdminSession(ctx, store.RevokeAdminSessionParams{ID: sess.ID, Reason: strPtr("mfa_completed")}); err != nil {
			return err
		}
		token, exp, err := s.createSession(ctx, q, u.ID, store.AdminSessionStageFull, s.cfg.AbsoluteTimeout, client)
		if err != nil {
			return err
		}
		if err := q.RecordAdminLoginSuccess(ctx, store.RecordAdminLoginSuccessParams{ID: u.ID, Now: s.now()}); err != nil {
			return err
		}
		remaining, err := q.CountAdminRecoveryCodes(ctx, u.ID)
		if err != nil {
			return err
		}
		out = Session{Token: token, ExpiresAt: exp, Principal: Principal{
			UserID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role, ExpiresAt: exp, IdleExpiresAt: s.idleExpiry(s.now(), exp),
		}}
		return audit.Record(ctx, q, audit.Entry{Actor: actor, Action: "admin.signed_in", EntityType: "admin_user", EntityID: u.ID.String(),
			Details: map[string]any{"ip": client.ipString(), "used_recovery_code": usedRecovery, "recovery_codes_left": remaining}})
	})
	if err != nil {
		return Session{}, err
	}
	s.logger.InfoContext(ctx, "staff signed in", slog.String("admin_user_id", u.ID.String()), slog.Bool("used_recovery_code", usedRecovery))
	return out, nil
}

func (s *Service) idleExpiry(lastSeen, absolute time.Time) time.Time {
	idle := lastSeen.Add(s.cfg.IdleTimeout)
	if idle.After(absolute) {
		return absolute
	}
	return idle
}

// Authenticate resolves a session token to its principal, enforcing the idle
// and absolute timeouts and the account's current status and role.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if !tokenRe.MatchString(token) {
		return Principal{}, ErrSessionInvalid
	}
	q := store.New(s.pool)
	sess, err := q.GetAdminSessionByHash(ctx, digest(token))
	if err != nil {
		if db.IsNotFound(err) {
			return Principal{}, ErrSessionInvalid
		}
		return Principal{}, err
	}
	now := s.now()
	if sess.Stage != store.AdminSessionStageFull || sess.RevokedAt != nil || sess.UserStatus != store.AdminUserStatusActive {
		return Principal{}, ErrSessionInvalid
	}
	if !now.Before(sess.ExpiresAt) {
		_ = q.RevokeAdminSession(ctx, store.RevokeAdminSessionParams{ID: sess.ID, Reason: strPtr("expired")})
		return Principal{}, ErrSessionInvalid
	}
	if now.Sub(sess.LastSeenAt) > s.cfg.IdleTimeout {
		_ = q.RevokeAdminSession(ctx, store.RevokeAdminSessionParams{ID: sess.ID, Reason: strPtr("idle_timeout")})
		return Principal{}, ErrSessionInvalid
	}
	if err := q.TouchAdminSession(ctx, store.TouchAdminSessionParams{ID: sess.ID, Now: now}); err != nil {
		return Principal{}, err
	}
	return Principal{
		UserID: sess.UserID, Email: sess.Email, Name: sess.Name, Role: sess.Role, SessionID: sess.ID,
		ExpiresAt: sess.ExpiresAt, IdleExpiresAt: s.idleExpiry(now, sess.ExpiresAt),
	}, nil
}

// Logout revokes the session behind token. Unknown tokens are ignored.
func (s *Service) Logout(ctx context.Context, token string) error {
	if !tokenRe.MatchString(token) {
		return nil
	}
	return db.WithTx(ctx, s.pool, "logout", func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		sess, err := q.GetAdminSessionByHash(ctx, digest(token))
		if err != nil {
			if db.IsNotFound(err) {
				return nil
			}
			return err
		}
		if sess.RevokedAt != nil {
			return nil
		}
		if err := q.RevokeAdminSession(ctx, store.RevokeAdminSessionParams{ID: sess.ID, Reason: strPtr("logout")}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Actor: audit.Admin(sess.UserID), Action: "admin.signed_out",
			EntityType: "admin_user", EntityID: sess.UserID.String()})
	})
}

// ChangePassword verifies the current password, sets a new one and signs the
// user out everywhere (including this session).
func (s *Service) ChangePassword(ctx context.Context, p Principal, current, next string, client Client) error {
	q := store.New(s.pool)
	u, err := q.GetAdminUser(ctx, p.UserID)
	if err != nil {
		return err
	}
	if u.PasswordHash == nil || len(current) > maxPasswordLen*4 {
		return ErrInvalidCredentials
	}
	ok, _, err := s.hash.Verify(ctx, current, *u.PasswordHash)
	if err != nil {
		return err
	}
	if !ok {
		s.recordFailure(ctx, q, u.ID, "wrong_current_password", client)
		return ErrInvalidCredentials
	}
	if err := CheckPasswordPolicy(next, u.Email, u.Name); err != nil {
		return err
	}
	if next == current {
		return weakPasswordError{"The new password must be different from the current one."}
	}
	phc, err := s.hash.Hash(ctx, next)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, s.pool, p.Actor().String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		if err := q.UpdateAdminPassword(ctx, store.UpdateAdminPasswordParams{ID: u.ID, PasswordHash: &phc}); err != nil {
			return err
		}
		if err := q.RevokeAdminUserSessions(ctx, store.RevokeAdminUserSessionsParams{UserID: u.ID, Reason: strPtr("password_changed")}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Actor: p.Actor(), Action: "admin_user.password_changed", EntityType: "admin_user",
			EntityID: u.ID.String(), Details: map[string]any{"ip": client.ipString()}})
	})
}

// ---------------------------------------------------------------------------
// Staff management
// ---------------------------------------------------------------------------

// ListUsers returns every staff account.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := store.New(s.pool).ListAdminUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]User, len(rows))
	for i, r := range rows {
		out[i] = userFromRow(r)
	}
	return out, nil
}

// GetUser returns one staff account.
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	u, err := store.New(s.pool).GetAdminUser(ctx, id)
	if err != nil {
		if db.IsNotFound(err) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	return userFromRow(u), nil
}

// GetUserByEmail returns one staff account by email (case-insensitive).
func (s *Service) GetUserByEmail(ctx context.Context, email string) (User, error) {
	u, err := store.New(s.pool).GetAdminUserByEmail(ctx, normalizeEmail(email))
	if err != nil {
		if db.IsNotFound(err) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	return userFromRow(u), nil
}

// lockForChange locks the target and refuses changes that would remove the
// last active admin.
func lockForChange(ctx context.Context, q *store.Queries, userID uuid.UUID, removesAdmin func(store.AdminUser) bool) (store.AdminUser, error) {
	u, err := q.LockAdminUser(ctx, userID)
	if err != nil {
		if db.IsNotFound(err) {
			return u, ErrNotFound
		}
		return u, err
	}
	if u.Role == store.AdminRoleAdmin && u.Status == store.AdminUserStatusActive && removesAdmin(u) {
		n, err := q.CountActiveAdmins(ctx)
		if err != nil {
			return u, err
		}
		if n <= 1 {
			return u, ErrLastAdmin
		}
	}
	return u, nil
}

// Disable offboards a staff member: they are signed out everywhere and can
// no longer sign in or use a pending setup link.
func (s *Service) Disable(ctx context.Context, actor audit.Actor, byUser *uuid.UUID, userID uuid.UUID) error {
	if byUser != nil && *byUser == userID {
		return ErrSelfAction
	}
	err := db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		if _, err := lockForChange(ctx, q, userID, func(store.AdminUser) bool { return true }); err != nil {
			return err
		}
		if _, err := q.DisableAdminUser(ctx, store.DisableAdminUserParams{ID: userID, DisabledBy: strPtr(actor.String())}); err != nil {
			if db.IsNotFound(err) {
				return ErrAlreadyDisabled
			}
			return err
		}
		if err := q.RevokeAdminUserSessions(ctx, store.RevokeAdminUserSessionsParams{UserID: userID, Reason: strPtr("account_disabled")}); err != nil {
			return err
		}
		if err := q.RevokeAdminInvitations(ctx, userID); err != nil {
			return err
		}
		if err := q.RevokeAdminRecoveryCodes(ctx, userID); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Actor: actor, Action: "admin_user.disabled", EntityType: "admin_user", EntityID: userID.String()})
	})
	if err == nil {
		s.logger.InfoContext(ctx, "staff user disabled", slog.String("admin_user_id", userID.String()))
	}
	return err
}

// SetRole changes a staff member's role. It takes effect on their next
// request (roles are read from the database on every request).
func (s *Service) SetRole(ctx context.Context, actor audit.Actor, byUser *uuid.UUID, userID uuid.UUID, role store.AdminRole) (User, error) {
	if !role.Valid() {
		return User{}, fmt.Errorf("%w: unknown role", ErrInvalidInput)
	}
	if byUser != nil && *byUser == userID {
		return User{}, ErrSelfAction
	}
	var out User
	err := db.WithTx(ctx, s.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		before, err := lockForChange(ctx, q, userID, func(store.AdminUser) bool { return role != store.AdminRoleAdmin })
		if err != nil {
			return err
		}
		u, err := q.SetAdminUserRole(ctx, store.SetAdminUserRoleParams{ID: userID, Role: role})
		if err != nil {
			return err
		}
		out = userFromRow(u)
		return audit.Record(ctx, q, audit.Entry{Actor: actor, Action: "admin_user.role_changed", EntityType: "admin_user",
			EntityID: userID.String(), Details: map[string]any{"from": string(before.Role), "to": string(role)}})
	})
	return out, err
}

func strPtr(s string) *string { return &s }
