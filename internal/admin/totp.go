package admin

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP uses HMAC-SHA1; authenticator apps expect it. HMAC-SHA1 is not affected by SHA-1 collisions.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

// TOTP per RFC 6238: HMAC-SHA1, 30-second steps, 6 digits, the parameters
// every authenticator app supports. Codes from the previous, current and next
// step are accepted (clock drift), and a step can be used only once
// (ClaimTOTPStep in the database), so a captured code cannot be replayed.

const (
	totpPeriod     = 30
	totpDigits     = 6
	totpSkew       = 1
	totpSecretSize = 20 // 160 bits, RFC 4226 recommendation
	totpIssuer     = "Talksasa Pay"
)

var totpB32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func totpStep(t time.Time) int64 { return t.Unix() / totpPeriod }

func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, bin%1_000_000)
}

// verifyTOTP returns the matched step if code is valid at now (±skew) and
// later than lastStep. Every candidate is compared, in constant time, so
// timing does not reveal which step matched.
func verifyTOTP(secret []byte, code string, now time.Time, lastStep *int64) (int64, bool) {
	if len(code) != totpDigits {
		return 0, false
	}
	cur := totpStep(now)
	var matched int64
	ok := false
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		s := cur + d
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, s)), []byte(code)) == 1 {
			if lastStep == nil || s > *lastStep {
				matched, ok = s, true
			}
		}
	}
	return matched, ok
}

// otpauthURI is what the QR code encodes for authenticator apps.
func otpauthURI(secret []byte, email string) string {
	label := url.PathEscape(totpIssuer + ":" + email)
	q := url.Values{}
	q.Set("secret", totpB32.EncodeToString(secret))
	q.Set("issuer", totpIssuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
