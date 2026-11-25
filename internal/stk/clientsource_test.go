package stk

import (
	"context"
	"errors"
	"testing"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
)

type switchable struct {
	mpesa.Client
	on      bool
	paybill string
}

func (s *switchable) Configured() bool         { return s.on }
func (s *switchable) PaybillShortCode() string { return s.paybill }

func TestClientSourceFollowsSettings(t *testing.T) {
	src := &switchable{}
	s := NewService(Options{Client: src, ShortCode: "111111"})
	if s.Available() || s.paybillShortCode() != "111111" {
		t.Fatal("unconfigured source reported available")
	}
	if _, _, err := s.Send(context.Background(), audit.System("test"), SendParams{Phone: "0712345678", Source: SourcePayPage}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("send while off: %v", err)
	}
	src.on, src.paybill = true, "600222"
	if !s.Available() || s.paybillShortCode() != "600222" {
		t.Fatal("did not follow the source")
	}
}
