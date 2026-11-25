package stk

import "github.com/talksasa/talksasa-pay/internal/mpesa"

// ClientSource is a Daraja client whose settings can change while the
// service runs (*mpesasettings.Provider: the paybill an admin saves in the
// panel). When Options.Client implements it, Available follows
// Configured, and C2B confirmations are checked against PaybillShortCode
// instead of Options.ShortCode.
type ClientSource interface {
	mpesa.Client
	// Configured reports whether prompts can be sent right now.
	Configured() bool
	// PaybillShortCode is the paybill in effect ("" = not checked).
	PaybillShortCode() string
}

// available: a client is set and, for a ClientSource, configured.
func (s *Service) available() bool {
	if s.o.Client == nil {
		return false
	}
	if cs, ok := s.o.Client.(ClientSource); ok {
		return cs.Configured()
	}
	return true
}

// paybillShortCode is the only shortcode whose C2B confirmations are
// recorded ("" = any).
func (s *Service) paybillShortCode() string {
	if cs, ok := s.o.Client.(ClientSource); ok {
		if sc := cs.PaybillShortCode(); sc != "" {
			return sc
		}
	}
	return s.o.ShortCode
}
