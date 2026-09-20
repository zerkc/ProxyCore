package dbexport

import "os"

func testDatabaseURL() (string, bool) {
	if url := os.Getenv("PGX_TEST_DATABASE_URL"); url != "" {
		return url, true
	}
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url, true
	}
	return "", false
}
