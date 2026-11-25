package mpesasettings

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/talksasa/talksasa-pay/internal/config"
)

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRedactToken(t *testing.T) {
	if got := RedactToken("https://x/cb/mpesa/"+cbToken+"/stk", cbToken); got != "https://x/cb/mpesa/[TOKEN]/stk" {
		t.Fatal(got)
	}
	if got := RedactToken("https://x/cb/mpesa/a%2Fb/stk", "a/b"); strings.Contains(got, "a%2Fb") {
		t.Fatal(got)
	}
	if got := RedactToken("https://x/", ""); got != "https://x/" {
		t.Fatal(got)
	}
}

func TestCredentialHint(t *testing.T) {
	if c := credential(config.NewSecret("short")); !c.Set || c.Last4 != "" {
		t.Fatalf("short values get no hint: %+v", c)
	}
	if c := credential(config.NewSecret("abcdefgh1234")); c.Last4 != "1234" {
		t.Fatalf("%+v", c)
	}
	if c := credential(config.Secret{}); c.Set {
		t.Fatal("empty is not set")
	}
}

// Settings print no secret through any formatting path.
func TestSettingsNeverFormatSecrets(t *testing.T) {
	st := Settings{ConsumerKey: config.NewSecret(conKey), ConsumerSecret: config.NewSecret(conSecret), Passkey: config.NewSecret(passkey)}
	for _, s := range []string{fmt.Sprintf("%v", st), fmt.Sprintf("%+v", st), fmt.Sprintf("%#v", st)} {
		noSecrets(t, "formatted settings", s)
	}
}
