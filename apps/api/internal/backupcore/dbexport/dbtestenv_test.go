package dbexport

import "testing"

func TestTestDatabaseURL(t *testing.T) {
	tests := []struct {
		name        string
		pgxURL      string
		databaseURL string
		wantURL     string
		wantOK      bool
	}{
		{
			name:        "PGX only",
			pgxURL:      "postgres://pgx-only",
			databaseURL: "",
			wantURL:     "postgres://pgx-only",
			wantOK:      true,
		},
		{
			name:        "DATABASE only",
			pgxURL:      "",
			databaseURL: "postgres://database-only",
			wantURL:     "postgres://database-only",
			wantOK:      true,
		},
		{
			name:        "neither",
			pgxURL:      "",
			databaseURL: "",
			wantURL:     "",
			wantOK:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PGX_TEST_DATABASE_URL", tt.pgxURL)
			t.Setenv("DATABASE_URL", tt.databaseURL)

			gotURL, gotOK := testDatabaseURL()
			if gotURL != tt.wantURL || gotOK != tt.wantOK {
				t.Fatalf("testDatabaseURL() = (%q, %t), want (%q, %t)", gotURL, gotOK, tt.wantURL, tt.wantOK)
			}
		})
	}
}
