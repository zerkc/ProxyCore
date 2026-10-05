package configuration

import "github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"

// canonicalUUIDText returns the canonical lowercase hyphenated form of id when
// it is spelled in any way PostgreSQL's uuid input accepts (uppercase, braced,
// hyphen-less, 4-digit groups) and returns id unchanged otherwise, so a malformed
// id keeps failing at the same point and with the same text as before.
//
// Both stores call it once at the entry of AddRecord: records are keyed in the
// database by the canonical spelling, so every map or slice lookup against
// stored ids must use it too. The parser lives in sqlitedb (a pure function,
// already shared by the SQLite adapters and pinned against postgres:17); this
// package already links sqlitedb, so the PG path takes no new dependency.
func canonicalUUIDText(id string) string {
	if canonical, err := sqlitedb.ParseUUIDLookupText(id); err == nil {
		return canonical
	}
	return id
}

// CanonicalUUIDText is the exported form of canonicalUUIDText for callers outside
// this package (the HTTP handlers) that compare a path id against stored ids.
func CanonicalUUIDText(id string) string {
	return canonicalUUIDText(id)
}
