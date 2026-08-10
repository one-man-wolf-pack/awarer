package checkpoint_test

import (
	"reflect"
	"testing"

	"awarer/internal/domain/checkpoint"
)

// TestRepositoryExposesNoMaterializingReaders is a boundedness guard: the
// production checkpoint.Repository contract must stay stream-first. A reader that
// materializes a whole manifest (Get/List/Latest) or a whole history (a plain
// StoreHealth, or the adapter's StoreHealthAll hoisted onto this broad port) is a
// foot-gun — it invites a future hot-path caller to load an unbounded manifest or
// retain every header by name. The one consumer that genuinely needs full history
// declares its own narrow port instead. If someone adds a materializing reader to the
// interface, this test fails and forces the boundedness decision back into review.
func TestRepositoryExposesNoMaterializingReaders(t *testing.T) {
	forbidden := map[string]string{
		"Get":            "materializes a whole manifest; use Header + OpenManifest",
		"List":           "materializes every manifest; use StoreHealthNewest",
		"Latest":         "materializes a manifest; use StoreHealthNewest(ctx, 1)",
		"StoreHealth":    "hides whether the whole history is retained; use StoreHealthNewest",
		"StoreHealthAll": "retains every header on a broad port; a consumer that needs the adapter's full read declares its own narrow port",
	}
	typ := reflect.TypeOf((*checkpoint.Repository)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if why, bad := forbidden[name]; bad {
			t.Errorf("checkpoint.Repository must not expose %q: %s", name, why)
		}
	}
}
