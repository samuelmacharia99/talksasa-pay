package webhook

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The server signer must agree with the vectors the SDKs test against
// (Go client and PHP/Laravel package), so a receiver using either SDK
// accepts exactly what the worker sends.
func TestSignerMatchesSDKVectors(t *testing.T) {
	for _, path := range []string{
		"../../pkg/talksasapay/testdata/webhook_signature_vectors.json",
		"../../clients/php/tests/fixtures/webhook_signature_vectors.json",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			Signing struct {
				Secret    string `json:"secret"`
				Timestamp int64  `json:"timestamp"`
				Payload   string `json:"payload"`
				V1        string `json:"v1"`
				Header    string `json:"header"`
			} `json:"signing_example"`
			Rotation *struct {
				Secrets   []string `json:"secrets"`
				Timestamp int64    `json:"timestamp"`
				Payload   string   `json:"payload"`
				Header    string   `json:"header"`
			} `json:"rotation_signing_example"`
			Vectors []struct {
				Name      string   `json:"name"`
				Secrets   []string `json:"secrets"`
				Payload   string   `json:"payload"`
				Header    *string  `json:"header"`
				Now       int64    `json:"now"`
				Tolerance int64    `json:"tolerance"`
				Expected  string   `json:"expected"`
			} `json:"vectors"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		s := f.Signing
		if got := Sign([]byte(s.Secret), s.Timestamp, []byte(s.Payload)); got != s.V1 {
			t.Errorf("%s: Sign = %s, want %s", path, got, s.V1)
		}
		if got := SignatureHeader(s.Timestamp, []byte(s.Payload), []byte(s.Secret)); got != s.Header {
			t.Errorf("%s: header = %s, want %s", path, got, s.Header)
		}
		if r := f.Rotation; r != nil {
			secrets := make([][]byte, len(r.Secrets))
			for i, x := range r.Secrets {
				secrets[i] = []byte(x)
			}
			if got := SignatureHeader(r.Timestamp, []byte(r.Payload), secrets...); got != r.Header {
				t.Errorf("%s: rotation header = %s, want %s", path, got, r.Header)
			}
		}
		for _, v := range f.Vectors {
			header := ""
			if v.Header != nil {
				header = *v.Header
			}
			ok := false
			for _, sec := range v.Secrets {
				if Verify([]byte(sec), header, []byte(v.Payload), time.Unix(v.Now, 0), time.Duration(v.Tolerance)*time.Second) {
					ok = true
				}
			}
			if ok != (v.Expected == "valid") {
				t.Errorf("%s: %q: server Verify=%v, SDK expects %s", path, v.Name, ok, v.Expected)
			}
		}
	}
}
