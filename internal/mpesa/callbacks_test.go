package mpesa

import (
	"errors"
	"testing"
	"time"
)

const stkOK = `{"Body":{"stkCallback":{"MerchantRequestID":"29115-34620561-1","CheckoutRequestID":"ws_CO_191220191020363925",
"ResultCode":0,"ResultDesc":"The service request is processed successfully.","CallbackMetadata":{"Item":[
{"Name":"Amount","Value":1.00},{"Name":"MpesaReceiptNumber","Value":"nlj7rt61sv"},{"Name":"Balance"},
{"Name":"TransactionDate","Value":20191219102115},{"Name":"PhoneNumber","Value":254708374149}]}}}}`

func TestParseSTKCallbackSuccess(t *testing.T) {
	r, err := ParseSTKCallback([]byte(stkOK))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Succeeded() || r.Receipt != "NLJ7RT61SV" || r.Amount != 1 || r.Phone != "254708374149" ||
		r.CheckoutRequestID != "ws_CO_191220191020363925" || r.MerchantRequestID != "29115-34620561-1" {
		t.Fatalf("%+v", r)
	}
	want := time.Date(2019, 12, 19, 10, 21, 15, 0, Nairobi)
	if r.OccurredAt == nil || !r.OccurredAt.Equal(want) {
		t.Fatalf("occurred %v", r.OccurredAt)
	}
}

func TestParseSTKCallbackFailure(t *testing.T) {
	r, err := ParseSTKCallback([]byte(`{"Body":{"stkCallback":{"MerchantRequestID":"m","CheckoutRequestID":"c","ResultCode":1032,"ResultDesc":"Request cancelled by user"}}}`))
	if err != nil || r.Succeeded() || r.ResultCode != ResultCancelledByUser || r.ResultDesc != "Request cancelled by user" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestParseSTKCallbackRejects(t *testing.T) {
	cases := map[string]struct {
		body string
		want error
	}{
		"not json":       {`{`, ErrMalformed},
		"no callback":    {`{"Body":{}}`, ErrMalformed},
		"no checkout id": {`{"Body":{"stkCallback":{"ResultCode":0}}}`, ErrMalformed},
		"no metadata":    {`{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0}}}`, ErrNoReceipt},
		"no receipt": {`{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0,"CallbackMetadata":{"Item":[{"Name":"Amount","Value":5}]}}}}`,
			ErrNoReceipt},
		"fraction": {`{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0,"CallbackMetadata":{"Item":[{"Name":"Amount","Value":5.5},{"Name":"MpesaReceiptNumber","Value":"ABC"}]}}}}`,
			ErrBadAmount},
		"zero": {`{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0,"CallbackMetadata":{"Item":[{"Name":"Amount","Value":0},{"Name":"MpesaReceiptNumber","Value":"ABC"}]}}}}`,
			ErrBadAmount},
		"no amount": {`{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0,"CallbackMetadata":{"Item":[{"Name":"MpesaReceiptNumber","Value":"ABC"}]}}}}`,
			ErrBadAmount},
		"bad receipt": {`{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0,"CallbackMetadata":{"Item":[{"Name":"Amount","Value":5},{"Name":"MpesaReceiptNumber","Value":"AB-1"}]}}}}`,
			ErrBadReceipt},
	}
	for name, c := range cases {
		if _, err := ParseSTKCallback([]byte(c.body)); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

func TestParseC2BConfirmation(t *testing.T) {
	c, err := ParseC2BConfirmation([]byte(`{"TransactionType":"Pay Bill","TransID":"rktqdm7w6s","TransTime":"20191122063845","TransAmount":"10.00",
"BusinessShortCode":"600638","BillRefNumber":" host-1042 ","InvoiceNumber":"","OrgAccountBalance":"49197.00","ThirdPartyTransID":"",
"MSISDN":"2547 ***** 126","FirstName":"John","MiddleName":"","LastName":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Receipt != "RKTQDM7W6S" || c.Amount != 10 || c.BillRef != "host-1042" || c.ShortCode != "600638" || c.Phone != "" || c.OccurredAt == nil {
		t.Fatalf("%+v", c)
	}
	c, err = ParseC2BConfirmation([]byte(`{"TransID":"X1","TransAmount":250000,"MSISDN":254712345678}`))
	if err != nil || c.Amount != 250000 || c.Phone != "254712345678" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, amt := range []string{`"10.50"`, `"-1"`, `"0"`, `"250001"`, `""`, `"1e3"`} {
		if _, err := ParseC2BConfirmation([]byte(`{"TransID":"X1","TransAmount":` + amt + `}`)); !errors.Is(err, ErrBadAmount) {
			t.Errorf("amount %s: %v", amt, err)
		}
	}
	if _, err := ParseC2BConfirmation([]byte(`{"TransAmount":"1"}`)); !errors.Is(err, ErrNoReceipt) {
		t.Errorf("no receipt: %v", err)
	}
}
