package runcache_test

import (
	"reflect"
	"testing"

	"awarer/internal/domain/runcache"
)

// TestStoreExposesNoMaterializingList is a boundedness guard: the production
// runcache.Store contract must stay stream-first. A neutral List() that
// materializes and validates every run entry is a foot-gun for a large run
// history; production listers stream ids (ListRefs/ListRefsNewest/IterRefsNewest)
// and decode metadata on demand. Adding List() here fails this test and forces the
// decision back into review.
func TestStoreExposesNoMaterializingList(t *testing.T) {
	typ := reflect.TypeOf((*runcache.Store)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		if name := typ.Method(i).Name; name == "List" {
			t.Errorf("runcache.Store must not expose %q: it materializes every entry; use ListRefs/ListRefsNewest/IterRefsNewest", name)
		}
	}
}
