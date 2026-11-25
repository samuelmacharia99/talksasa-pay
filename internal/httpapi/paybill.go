package httpapi

// paybill is the paybill in effect ("" hides the instructions).
func (d Deps) paybill() string {
	if d.PaybillFrom != nil {
		return d.PaybillFrom()
	}
	return d.Paybill
}

// mountCallbacks: the Safaricom callback routes exist while M-Pesa is on,
// or, when its settings can change at run time (PaybillFrom set: saved in
// the admin panel), whenever a callback token is configured, so a paybill
// saved later works without a restart.
func mountCallbacks(d Deps) bool {
	if d.STK == nil {
		return false
	}
	return d.STK.Available() || (d.PaybillFrom != nil && !d.MpesaCallbackToken.IsZero())
}
