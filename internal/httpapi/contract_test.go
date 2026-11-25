package httpapi

import (
	"testing"

	"github.com/talksasa/talksasa-pay/internal/testutil/specshape"
)

// The public pay page returns exactly the fields api/public.openapi.yaml
// documents: nothing else (ids, phone, metadata) can reach a visitor.
func TestPayPageShapeMatchesSpec(t *testing.T) {
	const spec = "../../api/public.openapi.yaml"
	specshape.Match(t, spec, "PayPage", payPageJSON{Paybill: &paybillJSON{}, STKPush: &payPageSTK{}})
	specshape.Match(t, spec, "PayPage", payPageJSON{})
	specshape.Match(t, spec, "PayPageSTK", payPageSTK{})
}
