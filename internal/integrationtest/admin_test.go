package integrationtest

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" // independent RFC 6238 implementation for tests
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/admin"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// fakeClock lets tests move time forward for timeouts and lockouts.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type adminEnv struct {
	*env
	svc   *admin.Service
	clock *fakeClock
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	e := newEnv(t)
	clock := &fakeClock{now: time.Now().UTC()}
	cfg := admin.DefaultConfig()
	cfg.SetupBaseURL = "https://admin.test/setup/"
	svc := admin.NewService(admin.Options{
		Pool: e.db.App, Keyring: e.keyring, Pepper: []byte(strings.Repeat("p", 40)),
		Config: cfg, Logger: e.logger, Now: clock.Now,
	})
	return &adminEnv{env: e, svc: svc, clock: clock}
}

// totp is an independent RFC 6238 implementation, so the tests do not trust
// the code under test to generate its own expected values.
func totp(t *testing.T, secretB32 string, at time.Time) string {
	t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secretB32)
	if err != nil {
		t.Fatal(err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[off:off+4])&0x7fffffff)%1_000_000)
}

var client = admin.Client{IP: ptrAddr(netip.MustParseAddr("203.0.113.7")), UserAgent: "integration-test"}

func ptrAddr(a netip.Addr) *netip.Addr { return &a }

const goodPassword = "purple-otter-kettle-71"

type staff struct {
	id       uuid.UUID
	email    string
	secret   string
	recovery []string
}

// onboard invites and fully activates a staff member.
func (a *adminEnv) onboard(email string, role store.AdminRole) staff {
	a.t.Helper()
	inv, err := a.svc.Invite(a.ctx, operator, email, "Test "+strings.Split(email, "@")[0], role)
	if err != nil {
		a.t.Fatalf("invite: %v", err)
	}
	info, err := a.svc.BeginSetup(a.ctx, inv.Token)
	if err != nil {
		a.t.Fatalf("begin setup: %v", err)
	}
	codes, err := a.svc.CompleteSetup(a.ctx, inv.Token, info.Enrolment, goodPassword, totp(a.t, info.Secret, a.clock.Now()), client)
	if err != nil {
		a.t.Fatalf("complete setup: %v", err)
	}
	return staff{id: inv.User.ID, email: inv.User.Email, secret: info.Secret, recovery: codes}
}

// signIn performs password + TOTP. It first moves the clock to a fresh TOTP
// step, because each step can be used only once.
func (a *adminEnv) signIn(s staff) admin.Session {
	a.t.Helper()
	a.clock.Advance(30 * time.Second)
	ch, err := a.svc.Login(a.ctx, s.email, goodPassword, client)
	if err != nil {
		a.t.Fatalf("login: %v", err)
	}
	sess, err := a.svc.VerifyMFA(a.ctx, ch.Token, totp(a.t, s.secret, a.clock.Now()), client)
	if err != nil {
		a.t.Fatalf("verify mfa: %v", err)
	}
	return sess
}

func TestAdminInviteSetupSignInLifecycle(t *testing.T) {
	a := newAdminEnv(t)
	inv, err := a.svc.Invite(a.ctx, operator, "  Jane.Doe@Talksasa.co.ke ", "Jane Doe", store.AdminRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if inv.User.Email != "jane.doe@talksasa.co.ke" || inv.User.Status != store.AdminUserStatusInvited {
		t.Fatalf("unexpected user %+v", inv.User)
	}
	if !strings.HasPrefix(inv.URL, "https://admin.test/setup/") || !strings.HasSuffix(inv.URL, inv.Token) {
		t.Fatalf("setup URL %q", inv.URL)
	}
	if _, err := a.svc.Invite(a.ctx, operator, "JANE.DOE@talksasa.co.ke", "Dup", store.AdminRoleViewer); !errors.Is(err, admin.ErrEmailTaken) {
		t.Fatalf("duplicate email: %v", err)
	}
	for _, bad := range []string{"", "nope", strings.Repeat("a", 52), inv.Token + "x"} {
		if _, err := a.svc.BeginSetup(a.ctx, bad); !errors.Is(err, admin.ErrInviteInvalid) {
			t.Fatalf("BeginSetup(%q): %v", bad, err)
		}
	}
	info, err := a.svc.BeginSetup(a.ctx, inv.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.OTPAuthURI, "otpauth://totp/") || info.Email != inv.User.Email {
		t.Fatalf("setup info %+v", info)
	}
	code := totp(t, info.Secret, a.clock.Now())

	if _, err := a.svc.CompleteSetup(a.ctx, inv.Token, info.Enrolment, "short", code, client); !errors.Is(err, admin.ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	wrong := fmt.Sprintf("%06d", (atoi(code)+1)%1_000_000)
	if _, err := a.svc.CompleteSetup(a.ctx, inv.Token, info.Enrolment, goodPassword, wrong, client); !errors.Is(err, admin.ErrInvalidCode) {
		t.Fatalf("wrong code: %v", err)
	}
	tampered := info.Enrolment[:len(info.Enrolment)-2] + "AA"
	if _, err := a.svc.CompleteSetup(a.ctx, inv.Token, tampered, goodPassword, code, client); !errors.Is(err, admin.ErrEnrolmentInvalid) {
		t.Fatalf("tampered enrolment: %v", err)
	}
	// An enrolment blob bound to another invitation is useless here.
	other, _ := a.svc.Invite(a.ctx, operator, "other@talksasa.co.ke", "Other Person", store.AdminRoleViewer)
	otherInfo, _ := a.svc.BeginSetup(a.ctx, other.Token)
	if _, err := a.svc.CompleteSetup(a.ctx, inv.Token, otherInfo.Enrolment, goodPassword, totp(t, otherInfo.Secret, a.clock.Now()), client); !errors.Is(err, admin.ErrEnrolmentInvalid) {
		t.Fatalf("cross-invitation enrolment: %v", err)
	}

	codes, err := a.svc.CompleteSetup(a.ctx, inv.Token, info.Enrolment, goodPassword, code, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 10 {
		t.Fatalf("got %d recovery codes", len(codes))
	}
	if _, err := a.svc.BeginSetup(a.ctx, inv.Token); !errors.Is(err, admin.ErrInviteInvalid) {
		t.Fatalf("setup link reusable: %v", err)
	}

	// Sign-in failures are indistinguishable.
	for _, c := range []struct{ email, pw string }{
		{"jane.doe@talksasa.co.ke", "wrong-password-123"},
		{"nobody@talksasa.co.ke", goodPassword},
		{"other@talksasa.co.ke", goodPassword}, // invited, not active
	} {
		if _, err := a.svc.Login(a.ctx, c.email, c.pw, client); !errors.Is(err, admin.ErrInvalidCredentials) {
			t.Fatalf("login(%s): %v", c.email, err)
		}
	}

	ch, err := a.svc.Login(a.ctx, "JANE.DOE@talksasa.co.ke", goodPassword, client)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-2FA token is not a session.
	if _, err := a.svc.Authenticate(a.ctx, ch.Token); !errors.Is(err, admin.ErrSessionInvalid) {
		t.Fatalf("challenge used as session: %v", err)
	}
	// The setup code's time step was consumed: replay is refused.
	if _, err := a.svc.VerifyMFA(a.ctx, ch.Token, code, client); !errors.Is(err, admin.ErrInvalidCode) {
		t.Fatalf("replayed TOTP: %v", err)
	}
	a.clock.Advance(30 * time.Second)
	fresh := totp(t, info.Secret, a.clock.Now())
	sess, err := a.svc.VerifyMFA(a.ctx, ch.Token, fresh, client)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Token == ch.Token {
		t.Fatal("session token not rotated after 2FA")
	}
	// The challenge is spent.
	if _, err := a.svc.VerifyMFA(a.ctx, ch.Token, fresh, client); !errors.Is(err, admin.ErrSessionInvalid) {
		t.Fatalf("challenge reusable: %v", err)
	}
	p, err := a.svc.Authenticate(a.ctx, sess.Token)
	if err != nil {
		t.Fatal(err)
	}
	if p.Role != store.AdminRoleAdmin || p.Email != "jane.doe@talksasa.co.ke" || !p.Can(admin.PermManageUsers) {
		t.Fatalf("principal %+v", p)
	}
	if err := a.svc.Logout(a.ctx, sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.Authenticate(a.ctx, sess.Token); !errors.Is(err, admin.ErrSessionInvalid) {
		t.Fatalf("session valid after logout: %v", err)
	}
	a.assertAudit("admin_user.invited", "admin_user.activated", "admin.signed_in", "admin.signed_out")
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func TestAdminLockoutAfterRepeatedFailures(t *testing.T) {
	a := newAdminEnv(t)
	s := a.onboard("lock@talksasa.co.ke", store.AdminRoleViewer)
	for i := 0; i < 5; i++ {
		if _, err := a.svc.Login(a.ctx, s.email, "wrong-password-xyz", client); !errors.Is(err, admin.ErrInvalidCredentials) {
			t.Fatal(err)
		}
	}
	// Locked: even the right password fails, with the same error.
	if _, err := a.svc.Login(a.ctx, s.email, goodPassword, client); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("locked account accepted: %v", err)
	}
	a.clock.Advance(61 * time.Second)
	sess := a.signIn(s) // lock expired; success resets the counter
	if _, err := a.svc.Authenticate(a.ctx, sess.Token); err != nil {
		t.Fatal(err)
	}
	// Wrong 2FA codes count too.
	ch, err := a.svc.Login(a.ctx, s.email, goodPassword, client)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, _ = a.svc.VerifyMFA(a.ctx, ch.Token, "000000", client)
	}
	a.clock.Advance(30 * time.Second)
	if _, err := a.svc.VerifyMFA(a.ctx, ch.Token, totp(t, s.secret, a.clock.Now()), client); err == nil {
		t.Fatal("2FA brute force not locked out")
	}
}

func TestAdminRecoveryCodeSingleUse(t *testing.T) {
	a := newAdminEnv(t)
	s := a.onboard("recover@talksasa.co.ke", store.AdminRoleFinance)
	ch, _ := a.svc.Login(a.ctx, s.email, goodPassword, client)
	if _, err := a.svc.VerifyMFA(a.ctx, ch.Token, strings.ToUpper(s.recovery[0]), client); err != nil {
		t.Fatalf("recovery code rejected: %v", err)
	}
	ch, _ = a.svc.Login(a.ctx, s.email, goodPassword, client)
	if _, err := a.svc.VerifyMFA(a.ctx, ch.Token, s.recovery[0], client); !errors.Is(err, admin.ErrInvalidCode) {
		t.Fatalf("recovery code reused: %v", err)
	}
	// Another user's code is useless.
	o := a.onboard("other2@talksasa.co.ke", store.AdminRoleViewer)
	if _, err := a.svc.VerifyMFA(a.ctx, ch.Token, o.recovery[0], client); !errors.Is(err, admin.ErrInvalidCode) {
		t.Fatalf("cross-user recovery code: %v", err)
	}
}

func TestAdminSessionTimeouts(t *testing.T) {
	a := newAdminEnv(t)
	s := a.onboard("timeout@talksasa.co.ke", store.AdminRoleViewer)

	idle := a.signIn(s)
	a.clock.Advance(16 * time.Minute)
	if _, err := a.svc.Authenticate(a.ctx, idle.Token); !errors.Is(err, admin.ErrSessionInvalid) {
		t.Fatalf("idle session still valid: %v", err)
	}

	active := a.signIn(s)
	for elapsed := time.Duration(0); elapsed < 7*time.Hour+50*time.Minute; elapsed += 10 * time.Minute {
		a.clock.Advance(10 * time.Minute)
		if _, err := a.svc.Authenticate(a.ctx, active.Token); err != nil {
			t.Fatalf("active session rejected after %v: %v", elapsed, err)
		}
	}
	a.clock.Advance(11 * time.Minute)
	if _, err := a.svc.Authenticate(a.ctx, active.Token); !errors.Is(err, admin.ErrSessionInvalid) {
		t.Fatalf("session outlived the 8h absolute timeout: %v", err)
	}

	ch, _ := a.svc.Login(a.ctx, s.email, goodPassword, client)
	a.clock.Advance(6 * time.Minute)
	if _, err := a.svc.VerifyMFA(a.ctx, ch.Token, totp(t, s.secret, a.clock.Now()), client); !errors.Is(err, admin.ErrSessionInvalid) {
		t.Fatalf("expired 2FA challenge accepted: %v", err)
	}
}

func TestAdminOffboardingAndLastAdminGuard(t *testing.T) {
	a := newAdminEnv(t)
	boss := a.onboard("boss@talksasa.co.ke", store.AdminRoleAdmin)
	clerk := a.onboard("clerk@talksasa.co.ke", store.AdminRoleViewer)
	bossActor := audit.Admin(boss.id)

	if err := a.svc.Disable(a.ctx, bossActor, &boss.id, boss.id); !errors.Is(err, admin.ErrSelfAction) {
		t.Fatalf("self-disable: %v", err)
	}
	if _, err := a.svc.SetRole(a.ctx, operator, nil, boss.id, store.AdminRoleViewer); !errors.Is(err, admin.ErrLastAdmin) {
		t.Fatalf("demoting last admin: %v", err)
	}
	if err := a.svc.Disable(a.ctx, operator, nil, boss.id); !errors.Is(err, admin.ErrLastAdmin) {
		t.Fatalf("disabling last admin: %v", err)
	}

	clerkSess := a.signIn(clerk)
	if _, err := a.svc.SetRole(a.ctx, bossActor, &boss.id, clerk.id, store.AdminRoleAdmin); err != nil {
		t.Fatal(err)
	}
	// Role changes apply on the next request, without signing in again.
	p, err := a.svc.Authenticate(a.ctx, clerkSess.Token)
	if err != nil || p.Role != store.AdminRoleAdmin {
		t.Fatalf("role not applied: %+v %v", p, err)
	}

	bossSess := a.signIn(boss)
	if err := a.svc.Disable(a.ctx, audit.Admin(clerk.id), &clerk.id, boss.id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.Authenticate(a.ctx, bossSess.Token); !errors.Is(err, admin.ErrSessionInvalid) {
		t.Fatalf("disabled user's session still valid: %v", err)
	}
	if _, err := a.svc.Login(a.ctx, boss.email, goodPassword, client); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("disabled user signed in: %v", err)
	}
	if err := a.svc.Disable(a.ctx, operator, nil, boss.id); !errors.Is(err, admin.ErrAlreadyDisabled) {
		t.Fatalf("double disable: %v", err)
	}
}

func TestAdminResetRequiresFreshSetup(t *testing.T) {
	a := newAdminEnv(t)
	s := a.onboard("reset@talksasa.co.ke", store.AdminRoleViewer)
	sess := a.signIn(s)
	inv, err := a.svc.Reset(a.ctx, operator, nil, s.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.Authenticate(a.ctx, sess.Token); !errors.Is(err, admin.ErrSessionInvalid) {
		t.Fatal("session survived reset")
	}
	if _, err := a.svc.Login(a.ctx, s.email, goodPassword, client); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatal("old password works after reset")
	}
	// A second reset invalidates the first link.
	inv2, err := a.svc.Reset(a.ctx, operator, nil, s.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.BeginSetup(a.ctx, inv.Token); !errors.Is(err, admin.ErrInviteInvalid) {
		t.Fatal("superseded setup link still works")
	}
	info, err := a.svc.BeginSetup(a.ctx, inv2.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.CompleteSetup(a.ctx, inv2.Token, info.Enrolment, "brand-new-passphrase-9", totp(t, info.Secret, a.clock.Now()), client); err != nil {
		t.Fatal(err)
	}
	// Old recovery codes died with the reset.
	a.clock.Advance(30 * time.Second)
	ch, err := a.svc.Login(a.ctx, s.email, "brand-new-passphrase-9", client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.VerifyMFA(a.ctx, ch.Token, s.recovery[1], client); !errors.Is(err, admin.ErrInvalidCode) {
		t.Fatal("pre-reset recovery code still works")
	}
	// Invitations expire.
	inv3, _ := a.svc.Reset(a.ctx, operator, nil, s.id)
	a.clock.Advance(25 * time.Hour)
	if _, err := a.svc.BeginSetup(a.ctx, inv3.Token); !errors.Is(err, admin.ErrInviteInvalid) {
		t.Fatal("expired setup link accepted")
	}
}

func TestAdminChangePasswordSignsOutEverywhere(t *testing.T) {
	a := newAdminEnv(t)
	s := a.onboard("pw@talksasa.co.ke", store.AdminRoleViewer)
	s1, s2 := a.signIn(s), a.signIn(s)
	p, _ := a.svc.Authenticate(a.ctx, s1.Token)
	if err := a.svc.ChangePassword(a.ctx, p, "not-the-password", "another-good-pass-42", client); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("wrong current password: %v", err)
	}
	if err := a.svc.ChangePassword(a.ctx, p, goodPassword, goodPassword, client); !errors.Is(err, admin.ErrWeakPassword) {
		t.Fatalf("same password: %v", err)
	}
	if err := a.svc.ChangePassword(a.ctx, p, goodPassword, "another-good-pass-42", client); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{s1.Token, s2.Token} {
		if _, err := a.svc.Authenticate(a.ctx, tok); !errors.Is(err, admin.ErrSessionInvalid) {
			t.Fatal("session survived password change")
		}
	}
}

func TestAdminTablesGuardedInDatabase(t *testing.T) {
	a := newAdminEnv(t)
	s := a.onboard("guard@talksasa.co.ke", store.AdminRoleViewer)
	ctx := context.Background()
	// Live rows of every kind, so each statement below has something to hit.
	sess := a.signIn(s)
	if _, err := a.svc.Login(a.ctx, s.email, goodPassword, client); err != nil { // leaves an mfa-stage session
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"DELETE FROM admin_users",
		"DELETE FROM admin_sessions",
		"UPDATE admin_users SET email = 'x@y.zz'",
		"UPDATE admin_users SET status = 'invited'",                    // must clear credentials
		"UPDATE admin_users SET password_hash = 'plaintext-password'",  // not an argon2id hash
		"UPDATE admin_sessions SET stage = 'full' WHERE stage = 'mfa'", // stage immutable
		"UPDATE admin_sessions SET expires_at = expires_at + interval '1 year'",
	} {
		for _, pool := range []struct {
			name string
			exec func(string) error
		}{
			{"app", func(q string) error { _, err := a.db.App.Exec(ctx, q); return err }},
			{"owner", func(q string) error { _, err := a.db.Owner.Exec(ctx, q); return err }},
		} {
			if err := pool.exec(stmt); err == nil {
				t.Errorf("%s role: %q succeeded", pool.name, stmt)
			}
		}
	}
	// Revoked sessions cannot be revived.
	_ = a.svc.Logout(a.ctx, sess.Token)
	if _, err := a.db.Owner.Exec(ctx, "UPDATE admin_sessions SET revoked_at = NULL, revoke_reason = NULL WHERE revoked_at IS NOT NULL"); err == nil {
		t.Error("revoked session revived")
	}
}

func TestAdminSecretsNeverStoredOrLogged(t *testing.T) {
	a := newAdminEnv(t)
	inv, _ := a.svc.Invite(a.ctx, operator, "secret@talksasa.co.ke", "Secret Keeper", store.AdminRoleViewer)
	info, _ := a.svc.BeginSetup(a.ctx, inv.Token)
	codes, err := a.svc.CompleteSetup(a.ctx, inv.Token, info.Enrolment, goodPassword, totp(t, info.Secret, a.clock.Now()), client)
	if err != nil {
		t.Fatal(err)
	}
	s := staff{id: inv.User.ID, email: inv.User.Email, secret: info.Secret}
	sess := a.signIn(s)

	needles := append([]string{goodPassword, info.Secret, inv.Token, sess.Token}, codes...)
	var dump strings.Builder
	for _, q := range []string{
		"SELECT row_to_json(u)::text FROM admin_users u",
		"SELECT row_to_json(s)::text FROM admin_sessions s",
		"SELECT row_to_json(i)::text FROM admin_invitations i",
		"SELECT row_to_json(r)::text FROM admin_recovery_codes r",
		"SELECT row_to_json(a)::text FROM audit_log a",
	} {
		rows, err := a.db.Owner.Query(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			dump.WriteString(line)
		}
		rows.Close()
	}
	logs := a.logs.String()
	for _, n := range needles {
		plain := strings.ReplaceAll(n, "-", "")
		if strings.Contains(dump.String(), n) || strings.Contains(dump.String(), plain) {
			t.Errorf("secret %q found in the database", n[:4]+"…")
		}
		if strings.Contains(logs, n) || strings.Contains(logs, plain) {
			t.Errorf("secret %q found in logs", n[:4]+"…")
		}
	}
}

func (a *adminEnv) assertAudit(actions ...string) {
	a.t.Helper()
	for _, act := range actions {
		var n int
		if err := a.db.Owner.QueryRow(context.Background(), "SELECT count(*) FROM audit_log WHERE action = $1", act).Scan(&n); err != nil {
			a.t.Fatal(err)
		}
		if n == 0 {
			a.t.Errorf("no audit entry %q", act)
		}
	}
	var bad int64
	if err := a.db.Owner.QueryRow(context.Background(), "SELECT coalesce(tsp_audit_verify(), 0)").Scan(&bad); err != nil || bad != 0 {
		a.t.Errorf("audit chain broken at %d (%v)", bad, err)
	}
}
