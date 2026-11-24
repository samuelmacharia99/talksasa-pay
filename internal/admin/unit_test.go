package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/internal/store"
)

// Cheap parameters keep unit tests fast; production uses defaultArgon.
var testArgon = argonParams{memoryKiB: 8 * 1024, time: 1, threads: 1, keyLen: 32, saltLen: 16}

func TestPasswordHashRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := newHasher(testArgon, 2)
	phc, err := h.Hash(ctx, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("unexpected PHC string %q", phc)
	}
	ok, rehash, err := h.Verify(ctx, "correct horse battery staple", phc)
	if err != nil || !ok || rehash {
		t.Fatalf("verify correct: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	ok, _, err = h.Verify(ctx, "correct horse battery stapl", phc)
	if err != nil || ok {
		t.Fatalf("verify wrong: ok=%v err=%v", ok, err)
	}
	// Two hashes of the same password differ (random salt).
	phc2, _ := h.Hash(ctx, "correct horse battery staple")
	if phc == phc2 {
		t.Fatal("salt not random")
	}
}

func TestPasswordNeedsRehashWhenParamsChange(t *testing.T) {
	ctx := context.Background()
	old := newHasher(testArgon, 1)
	phc, _ := old.Hash(ctx, "a long enough password")
	stronger := testArgon
	stronger.time = 2
	ok, rehash, err := newHasher(stronger, 1).Verify(ctx, "a long enough password", phc)
	if err != nil || !ok || !rehash {
		t.Fatalf("ok=%v rehash=%v err=%v", ok, rehash, err)
	}
}

func TestParsePHCRejectsTamperedHashes(t *testing.T) {
	for _, bad := range []string{
		"", "plaintext", "$argon2i$v=19$m=65536,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=18$m=65536,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=99999999,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA", // memory DoS
		"$argon2id$v=19$m=65536,t=999,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=4$!!$aGFzaGhhc2hoYXNoaGFzaA",
	} {
		if _, _, _, err := parsePHC(bad); err == nil {
			t.Errorf("parsePHC(%q) accepted", bad)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	cases := []struct {
		pw   string
		want bool
	}{
		{"short", false},
		{strings.Repeat("a", 20), false},
		{strings.Repeat("x", 129), false},
		{"jane.doe-is-great-2026", false}, // contains email local part
		{"wanjiku rocks the paybill", false},
		{"purple-otter-kettle-71", true},
		{"ñandú ñandú ñandú", true}, // unicode counts runes
	}
	for _, c := range cases {
		err := CheckPasswordPolicy(c.pw, "jane.doe@talksasa.co.ke", "Wanjiku Kamau")
		if (err == nil) != c.want {
			t.Errorf("policy(%q) err=%v, want ok=%v", c.pw, err, c.want)
		}
		if err != nil {
			if !errors.Is(err, ErrWeakPassword) {
				t.Errorf("policy error does not wrap ErrWeakPassword: %v", err)
			}
			if msg, ok := PasswordProblem(err); !ok || msg == "" {
				t.Errorf("no user-facing message for %v", err)
			}
		}
	}
}

// RFC 6238 Appendix B test vectors (SHA-1), truncated to 6 digits.
func TestTOTPVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, v := range []struct {
		unix int64
		code string
	}{
		{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"},
		{1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"},
	} {
		if got := totpCode(secret, totpStep(time.Unix(v.unix, 0))); got != v.code {
			t.Errorf("t=%d: got %s want %s", v.unix, got, v.code)
		}
	}
}

func TestVerifyTOTPWindowAndReplay(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1_800_000_000, 0)
	cur := totpStep(now)
	for d := int64(-1); d <= 1; d++ {
		step, ok := verifyTOTP(secret, totpCode(secret, cur+d), now, nil)
		if !ok || step != cur+d {
			t.Errorf("skew %d rejected", d)
		}
	}
	if _, ok := verifyTOTP(secret, totpCode(secret, cur-2), now, nil); ok {
		t.Error("code two steps old accepted")
	}
	if _, ok := verifyTOTP(secret, totpCode(secret, cur+2), now, nil); ok {
		t.Error("code two steps ahead accepted")
	}
	last := cur
	if _, ok := verifyTOTP(secret, totpCode(secret, cur), now, &last); ok {
		t.Error("replayed step accepted")
	}
	if _, ok := verifyTOTP(secret, totpCode(secret, cur-1), now, &last); ok {
		t.Error("older step accepted after a newer one was used")
	}
	if _, ok := verifyTOTP(secret, "12345", now, nil); ok {
		t.Error("5-digit code accepted")
	}
}

func TestOTPAuthURI(t *testing.T) {
	u := otpauthURI([]byte("12345678901234567890"), "jane@talksasa.co.ke")
	for _, want := range []string{"otpauth://totp/Talksasa%20Pay:jane@talksasa.co.ke?", "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "issuer=Talksasa+Pay", "digits=6", "period=30"} { // gitleaks:allow (test fixture)
		if !strings.Contains(u, want) {
			t.Errorf("URI %q missing %q", u, want)
		}
	}
}

func TestRBACMatrix(t *testing.T) {
	want := map[store.AdminRole]map[Permission]bool{
		store.AdminRoleViewer:   {PermRead: true},
		store.AdminRoleFinance:  {PermRead: true},
		store.AdminRoleApprover: {PermRead: true},
		store.AdminRoleAdmin:    {PermRead: true, PermManageProducts: true, PermManageUsers: true},
	}
	for _, role := range store.AllAdminRoleValues() {
		for _, perm := range AllPermissions {
			if got := Allows(role, perm); got != want[role][perm] {
				t.Errorf("Allows(%s, %s) = %v, want %v", role, perm, got, want[role][perm])
			}
		}
	}
	if Allows("superuser", PermRead) {
		t.Error("unknown role granted a permission")
	}
}

func TestRecoveryCodeFormat(t *testing.T) {
	s := &Service{pepper: []byte("pepper-pepper-pepper-pepper-pepper")}
	codes, hashes, err := s.newRecoveryCodes([16]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != recoveryCodeCount || len(hashes) != recoveryCodeCount {
		t.Fatalf("got %d codes", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 13 || c[6] != '-' || !recoveryRe.MatchString(normalizeRecovery(c)) || seen[c] {
			t.Errorf("bad or duplicate code %q", c)
		}
		seen[c] = true
		if normalizeRecovery(strings.ToUpper(" "+c+" ")) != normalizeRecovery(c) {
			t.Errorf("normalisation not case/space insensitive for %q", c)
		}
	}
}
