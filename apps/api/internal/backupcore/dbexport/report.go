package dbexport

// ExportReport describes each table attempted by Export. A report is returned
// even when one or more tables fail so callers can inspect partial progress.
type ExportReport struct {
	Tables []TableStats
}

// TableStats contains the output accounting for one table.
type TableStats struct {
	Name       string
	RowCount   int
	Bytes      int64
	DurationMS int64
	Error      error
}
