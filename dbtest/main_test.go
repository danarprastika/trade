package dbtest_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aitc/trade/dbtest"
)

// TestMain pins a per-run identifier nonce and states the database
// prerequisite out loud.
//
// Two facts make this suite a DISPOSABLE-DATABASE suite:
//
//  1. ledger.entry is append-only, so the tests that must observe a real COMMIT
//     (a balanced journal committing, an unbalanced journal failing to commit)
//     leave committed rows behind. Deleting them would require disabling
//     ledger_entry_append_only, i.e. weakening the control under test.
//  2. The audit chain tests require the September 2026 partition to be at
//     genesis, because a committed chain can never be rewound.
//
// The suite therefore reports committed rows rather than hiding them. Reset the
// database before a run:
//
//	db/migrate.ps1 -Container <container> -Reset
func TestMain(m *testing.M) {
	dbtest.SetRunNonce(time.Now().UTC().UnixNano())

	if dbtest.TestDatabaseURL() == "" {
		fmt.Fprintf(os.Stderr,
			"dbtest: %s is not set, so the database suite will SKIP every test.\n"+
				"Set it to a DISPOSABLE database that has just been migrated, for example:\n"+
				"  $env:AITC_TEST_DATABASE_URL='postgres://postgres:aitc_local_dev_only@localhost:55439/aitc?sslmode=disable'\n"+
				"  db\\migrate.ps1 -Container aitc-pg17 -Reset\n",
			dbtest.EnvTestDatabaseURL)
	}

	os.Exit(m.Run())
}
