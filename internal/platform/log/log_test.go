package log

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/talksasa/talksasa-pay/internal/config"
)

func TestRedaction(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, Options{Level: slog.LevelDebug, Service: "t"})
	l.Info("x",
		slog.String("password", "hunter2"), slog.String("consumer_secret", "s3cr3t"), slog.String("Passkey", "pk"),
		slog.String("SecurityCredential", "cred"), slog.String("authorization", "Bearer abc"), slog.String("hosted_token", "tok"),
		slog.String("phone", "254712345678"), slog.Any("db", config.NewSecret("postgres://u:pw@h/db")),
		slog.Group("daraja", slog.String("access_token", "atok")),
		slog.String("receipt", "SKL4ABCD12"),
	)
	out := buf.String()
	for _, leaked := range []string{"hunter2", "s3cr3t", `"pk"`, "cred\"", "Bearer abc", `"tok"`, "254712345678", "pw@h", "atok"} {
		if strings.Contains(out, leaked) {
			t.Errorf("leaked %q in %s", leaked, out)
		}
	}
	if !strings.Contains(out, "2547*****678") || !strings.Contains(out, "SKL4ABCD12") {
		t.Errorf("masking or normal field wrong: %s", out)
	}
}

func TestContextAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, Options{Level: slog.LevelInfo})
	ctx := WithAttrs(context.Background(), slog.String("request_id", "r-123"))
	ctx = WithAttrs(ctx, slog.String("app_id", "p-1"))
	l.InfoContext(ctx, "hello")
	if !strings.Contains(buf.String(), `"request_id":"r-123"`) || !strings.Contains(buf.String(), `"app_id":"p-1"`) {
		t.Fatalf("context attrs missing: %s", buf.String())
	}
}

func TestMaskPhone(t *testing.T) {
	if MaskPhone("254712345678") != "2547*****678" || MaskPhone("123") != "***" {
		t.Fatal("MaskPhone")
	}
}
