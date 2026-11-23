package config

import (
	"encoding/json"
	"log/slog"
)

// Secret holds sensitive configuration (passwords, keys, DSNs with
// credentials). Every formatting path (fmt, slog, JSON) prints a redaction
// marker, so a secret cannot leak through a careless log line or an API
// response. Call Reveal only at the point of use.
type Secret struct {
	value string
}

const redacted = "[REDACTED]"

// NewSecret wraps a sensitive value.
func NewSecret(v string) Secret { return Secret{value: v} }

// Reveal returns the plaintext. Keep the returned value short-lived.
func (s Secret) Reveal() string { return s.value }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.value == "" }

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON never emits the secret.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// MarshalText never emits the secret.
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
