package dbimport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
)

type tablePayload struct {
	name string
	rows []map[string]any
}

var configTableNames = func() map[string]struct{} {
	result := make(map[string]struct{}, len(dbexport.DefaultConfigTables))
	for _, table := range dbexport.DefaultConfigTables {
		result[table] = struct{}{}
	}
	return result
}()

func readTablePayloads(ctx context.Context, files map[string]zipextract.File, tableNames []string) ([]tablePayload, error) {
	if len(tableNames) != len(dbexport.DefaultConfigTables) {
		return nil, fmt.Errorf("backup contains %d config tables, want %d", len(tableNames), len(dbexport.DefaultConfigTables))
	}
	seen := make(map[string]struct{}, len(tableNames))
	payloads := make([]tablePayload, 0, len(tableNames))
	for _, table := range tableNames {
		if _, ok := configTableNames[table]; !ok {
			return nil, fmt.Errorf("unsupported backup table %q", table)
		}
		if _, ok := seen[table]; ok {
			return nil, fmt.Errorf("duplicate backup table %q", table)
		}
		seen[table] = struct{}{}
		path := "db/" + table + ".json"
		file, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("archive entry %q is missing", path)
		}
		data, err := readArchiveFile(ctx, file)
		if err != nil {
			return nil, fmt.Errorf("read table %q: %w", table, err)
		}
		rows, err := decodeTableRows(data)
		if err != nil {
			return nil, fmt.Errorf("decode table %q: %w", table, err)
		}
		payloads = append(payloads, tablePayload{name: table, rows: rows})
	}
	for path := range files {
		if !strings.HasPrefix(path, "db/") || !strings.HasSuffix(path, ".json") {
			continue
		}
		table := strings.TrimSuffix(strings.TrimPrefix(path, "db/"), ".json")
		if _, ok := seen[table]; !ok {
			return nil, fmt.Errorf("archive contains an unlisted config table %q", table)
		}
	}
	if len(seen) != len(dbexport.DefaultConfigTables) {
		return nil, fmt.Errorf("backup does not contain all config tables")
	}
	return payloads, nil
}

func decodeTableRows(data []byte) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("table payload must be a JSON array")
	}
	decoder := json.NewDecoder(strings.NewReader(string(trimmed)))
	decoder.UseNumber()
	var rows []map[string]any
	if err := decoder.Decode(&rows); err != nil {
		return nil, err
	}
	for index, row := range rows {
		if row == nil {
			return nil, fmt.Errorf("row %d must be a JSON object", index)
		}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON data")
		}
		return nil, err
	}
	return rows, nil
}

func quoteTableIdentifier(table string) string {
	return `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
}
