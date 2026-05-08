package memstore_test

import (
	"testing"

	"github.com/rbaliyan/postbox/internal/store"
	"github.com/rbaliyan/postbox/internal/store/memstore"
	"github.com/rbaliyan/postbox/internal/store/storetest"
)

func TestMemstore_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		return memstore.New()
	})
}
