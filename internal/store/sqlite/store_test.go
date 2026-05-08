package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/rbaliyan/postbox/internal/store"
	"github.com/rbaliyan/postbox/internal/store/sqlite"
	"github.com/rbaliyan/postbox/internal/store/storetest"
)

func TestSQLite_Conformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		t.Helper()
		dir := t.TempDir()
		return sqlite.New(filepath.Join(dir, "test.db"))
	})
}

func TestSQLite_InMemory(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		return sqlite.New(":memory:")
	})
}
