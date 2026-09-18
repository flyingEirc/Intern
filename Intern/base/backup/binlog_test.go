package backup

import (
	"testing"

	"github.com/pingcap/tidb/pkg/parser"
)

func TestRejectDDLThatDatabaseFilterWouldSilentlyOmit(t *testing.T) {
	for _, tc := range []struct{ query, schema string }{
		{"ALTER TABLE app.orders ADD COLUMN total INT", ""},
		{"DROP TABLE unrelated.old, app.orders", "maintenance"},
		{"ALTER TABLE APP.orders ADD COLUMN total INT", "maintenance"},
	} {
		if err := validateQuery(parser.New(), tc.query, tc.schema, "app", "recovered"); err == nil {
			t.Fatalf("unsafe filtered DDL accepted: %s", tc.query)
		}
	}
	if err := validateQuery(parser.New(), "ALTER TABLE orders ADD COLUMN total INT", "app", "app", "recovered"); err != nil {
		t.Fatalf("default-schema DDL should replay safely: %v", err)
	}
	if err := validateQuery(parser.New(), "DROP TABLE unrelated.old", "maintenance", "app", "recovered"); err != nil {
		t.Fatalf("unrelated filtered DDL rejected: %v", err)
	}
}

func TestRoutineDDLRequiresPreservedDatabaseName(t *testing.T) {
	query := "CREATE PROCEDURE p() SELECT 1"
	if err := validateQuery(parser.New(), query, "app", "app", "app"); err != nil {
		t.Fatalf("routine DDL misclassified: %v", err)
	}
	if err := validateQuery(parser.New(), query, "app", "app", "recovered"); err == nil {
		t.Fatal("stored program SQL silently renamed")
	}
}
