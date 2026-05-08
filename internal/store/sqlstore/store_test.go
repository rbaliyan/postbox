package sqlstore_test

import (
	"database/sql"
	"os"
	"testing"

	"github.com/rbaliyan/postbox/internal/store"
	"github.com/rbaliyan/postbox/internal/store/sqlstore"
	"github.com/rbaliyan/postbox/internal/store/storetest"
)

// TestSQLStore_Conformance runs the store conformance suite against a real
// PostgreSQL instance. Set POSTGRES_DSN to enable, e.g.:
//
//	POSTGRES_DSN="postgres://test:test@localhost:5433/test?sslmode=disable" go test ./...
//
// The driver must be registered before this test runs; this test file registers
// nothing and lets the caller pick a driver via blank-import in their CI setup.
func TestSQLStore_Conformance(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set")
	}
	driver := os.Getenv("POSTGRES_DRIVER")
	if driver == "" {
		driver = "postgres"
	}

	storetest.Run(t, func(t *testing.T) store.Store {
		t.Helper()
		// Open a fresh DB and truncate state between sub-tests so they are
		// independent. The DSN must point at a writable test database.
		db, err := sql.Open(driver, dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })

		// Reset state — the conformance suite assumes an empty store.
		for _, table := range []string{"nodes", "domains", "users"} {
			if _, err := db.Exec("DROP TABLE IF EXISTS " + table); err != nil {
				t.Fatalf("drop %s: %v", table, err)
			}
		}
		return sqlstore.NewFromDB(db)
	})
}
