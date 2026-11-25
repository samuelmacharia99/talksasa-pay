package apiview

// paybill is the paybill in effect ("" hides the instructions).
func (v View) paybill() string {
	if v.PaybillFrom != nil {
		return v.PaybillFrom()
	}
	return v.Paybill
}
