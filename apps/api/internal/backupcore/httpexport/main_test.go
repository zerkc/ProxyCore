package httpexport

import (
	"fmt"
	"os"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
)

func TestMain(m *testing.M) {
	// Hold the shared test-only gate across m.Run; release before os.Exit,
	// since os.Exit skips deferred (LIFO) cleanup.
	release, err := dbexport.AcquireImportTestGate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "httpexport test gate acquisition failed: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := release(); err != nil {
		fmt.Fprintf(os.Stderr, "httpexport test gate release failed: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
