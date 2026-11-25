package talksasapay

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The SDK must keep up with api/app.openapi.yaml: every operation has a
// Client method, every event type a model, every problem code a constant.

const specPath = "../../api/app.openapi.yaml"

// operationId -> Client method.
var specMethods = map[string]string{
	"whoami":           "Whoami",
	"createCharge":     "CreateCharge",
	"listCharges":      "ListCharges",
	"getCharge":        "GetCharge",
	"sendSTKPush":      "STKPush",
	"cancelCharge":     "CancelCharge",
	"getSTKPush":       "GetSTKPush",
	"listTransactions": "ListTransactions",
	"getTransaction":   "GetTransaction",
	"listEvents":       "ListEvents",
}

// Every Code constant, which must equal the spec's Problem.code enum.
var allCodes = []string{
	CodeUnauthorized, CodeIPNotAllowed, CodeAppSuspended, CodeRateLimited, CodeOverloaded, CodeBusy, CodeTimeout,
	CodeNotFound, CodeMethodNotAllowed, CodeValidationFailed, CodeInvalidJSON, CodeUnsupportedMediaType,
	CodePayloadTooLarge, CodeIdempotencyKeyRequired, CodeIdempotencyConflict, CodeDuplicateReference,
	CodeNotCancellable, CodeNotPayable, CodeAmountTooLarge, CodeSTKInProgress, CodeMpesaUnavailable,
	CodeMpesaError, CodeNotImplemented, CodeInternalError,
}

func readSpec(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	return string(b)
}

// enumAfter returns the "- item" lines of the first enum after anchor.
func enumAfter(t *testing.T, spec, anchor string) []string {
	t.Helper()
	i := strings.Index(spec, anchor)
	if i < 0 {
		t.Fatalf("spec has no %q", anchor)
	}
	rest := spec[i+len(anchor):]
	j := strings.Index(rest, "enum:")
	if j < 0 {
		t.Fatalf("no enum after %q", anchor)
	}
	item := regexp.MustCompile(`^\s+- ([a-z_.]+)\s*$`)
	var out []string
	for _, line := range strings.Split(rest[j+len("enum:"):], "\n")[1:] {
		m := item.FindStringSubmatch(line)
		if m == nil {
			break
		}
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("empty enum after %q", anchor)
	}
	slices.Sort(out)
	return out
}

func TestContractOperations(t *testing.T) {
	ops := regexp.MustCompile(`operationId:\s*(\w+)`).FindAllStringSubmatch(readSpec(t), -1)
	var got []string
	for _, m := range ops {
		got = append(got, m[1])
	}
	var want []string
	for op := range specMethods {
		want = append(want, op)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("spec operations %v, SDK maps %v", got, want)
	}
	ct := reflect.TypeFor[*Client]()
	for op, name := range specMethods {
		if _, ok := ct.MethodByName(name); !ok {
			t.Errorf("%s: no Client.%s", op, name)
		}
	}
}

func TestContractProblemCodes(t *testing.T) {
	spec := enumAfter(t, readSpec(t), "        code:\n")
	have := slices.Clone(allCodes)
	slices.Sort(have)
	if !slices.Equal(spec, have) {
		t.Fatalf("spec codes %v\nSDK codes  %v", spec, have)
	}
}

func TestContractEventTypes(t *testing.T) {
	spec := enumAfter(t, readSpec(t), "    EventType:\n")
	have := slices.Clone(EventTypes)
	slices.Sort(have)
	if !slices.Equal(spec, have) {
		t.Fatalf("spec event types %v\nSDK %v", spec, have)
	}

	charge, _ := json.Marshal(sampleCharge)
	stk, _ := json.Marshal(sampleSTK)
	txn, _ := json.Marshal(sampleTxn)
	received, _ := json.Marshal(with(sampleTxn, "duplicate_of_charge", testChargeID))
	decoders := map[string]func(Event) (any, error){
		"charge":      func(e Event) (any, error) { return e.Charge() },
		"stk":         func(e Event) (any, error) { return e.STKPush() },
		"received":    func(e Event) (any, error) { return e.PaymentReceived() },
		"transaction": func(e Event) (any, error) { return e.Transaction() },
	}
	for _, typ := range EventTypes {
		var data []byte
		var ok []string // decoders that must accept this type
		switch {
		case strings.HasPrefix(typ, "charge."):
			data, ok = charge, []string{"charge"}
		case strings.HasPrefix(typ, "stk_push."):
			data, ok = stk, []string{"stk"}
		case typ == EventPaymentReceived:
			data, ok = received, []string{"received", "transaction"}
		case typ == EventTransactionReversed:
			data, ok = txn, []string{"transaction"}
		default:
			t.Fatalf("%s: no model", typ)
		}
		ev := Event{ID: "e", Type: typ, Data: data}
		for name, dec := range decoders {
			v, err := dec(ev)
			if want := slices.Contains(ok, name); want != (err == nil) {
				t.Errorf("%s via %s: err=%v", typ, name, err)
			} else if want && reflect.ValueOf(v).Elem().FieldByName("ID").String() == "" {
				t.Errorf("%s via %s: empty ID", typ, name)
			}
		}
	}

	pr, err := Event{ID: "e", Type: EventPaymentReceived, Data: received}.PaymentReceived()
	if err != nil || pr.DuplicateOfCharge == nil || *pr.DuplicateOfCharge != testChargeID || pr.Amount != 1500 || *pr.Receipt != "TKA1B2C3D4" {
		t.Fatalf("%+v %v", pr, err)
	}
	if _, err := (Event{ID: "e", Type: "future.type", Data: []byte(`{}`)}).Charge(); err == nil {
		t.Fatal("unknown type decoded")
	}
}
