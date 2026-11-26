package httpapi

// paybill is the paybill in effect ("" hides the instructions).
func (d Deps) paybill() string {
	if d.PaybillFrom != nil {
		return d.PaybillFrom()
	}
	return d.Paybill
}

// mountCallbacks: the Safaricom callback routes exist while M-Pesa is on,
// or, when its settings can change at run time (saved in the admin panel),
// whenever a token can be in effect: a configured token, or a token source
// that follows the saved settings. So a paybill saved later works without
// a restart. Mounted routes still reject everything without a valid token,
// looking exactly like unknown paths.
func mountCallbacks(d Deps) bool {
	if d.STK == nil {
		return false
	}
	return d.STK.Available() || d.MpesaCallbackTokenFrom != nil || (d.PaybillFrom != nil && !d.MpesaCallbackToken.IsZero())
}
