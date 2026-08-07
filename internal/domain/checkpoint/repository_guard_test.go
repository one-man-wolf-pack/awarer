package checkpoint_test

import (
	"reflect"
	"testing"

	"awarer/internal/domain/checkpoint"
)

// TestRepositoryExposesNoMaterializingReaders is a boundedness guard: the
// production checkpoint.Repository contract must stay stream-first. A
// neutral-sounding reader that materializes a whole manifest (Get/List/Latest) is
// a foot-gun — it invites a future hot-path caller to load an unbounded manifest by
// name. If someone adds a materializing reader to the interface, this test fails and
// forces the boundedness decision back into review.
func TestRepositoryExposesNoMaterializingReaders(t *testing.T) {
	forbidden := map[string]string{
		"Get":    "materializes a whole manifest; use Header + OpenManifest",
		"List":   "materializes every manifest; use ListHeaders/StoreHealth",
		"Latest": "materializes a manifest; use LatestHeader",
	}
	typ := reflect.TypeOf((*checkpoint.Repository)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if why, bad := forbidden[name]; bad {
			t.Errorf("checkpoint.Repository must not expose %q: %s", name, why)
		}
	}
}
