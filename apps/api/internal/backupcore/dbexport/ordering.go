package dbexport

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultConfigTables is the explicit allowlist used when callers do not
// provide a narrower table set. It contains every durable configuration table;
// runtime and history tables are intentionally absent.
// The task prose says 15, while its authoritative table list contains 16;
// all explicitly listed durable configuration tables are retained here.
var DefaultConfigTables = []string{
	"installation_settings",
	"users",
	"secrets",
	"resolver_pools",
	"zones",
	"dns_records",
	"forwarding_rules",
	"stream_routes",
	"internal_ca",
	"internal_ca_enrollment_state",
	"certificates",
	"provider_connections",
	"config_revisions",
	"installation_identity",
	"cluster_keys",
	"node_state",
}

type MissingTablesError struct {
	Tables []string
}

func (e *MissingTablesError) Error() string {
	if e == nil {
		return "configured PostgreSQL tables are missing"
	}
	return fmt.Sprintf("configured PostgreSQL tables are missing: %s", strings.Join(e.Tables, ", "))
}

type ForeignKeyCycleError struct {
	Path []string
}

func (e *ForeignKeyCycleError) Error() string {
	if e == nil {
		return "foreign-key cycle detected"
	}
	return fmt.Sprintf("foreign-key cycle detected: %s", strings.Join(e.Path, " -> "))
}

// exportOrder retains the required package-level helper for the default table
// set. Exporter uses the method below so custom test/administrative subsets are
// ordered by the same information_schema-derived graph.
func exportOrder(ctx context.Context, pool *pgxpool.Pool, configured ...[]string) ([]string, error) {
	tables := DefaultConfigTables
	if len(configured) > 0 {
		tables = configured[0]
	}
	return exportOrderForTables(ctx, pool, tables)
}

func (e *Exporter) exportOrder(ctx context.Context) ([]string, error) {
	if e == nil {
		return nil, fmt.Errorf("export order: nil exporter")
	}
	return exportOrderForTables(ctx, e.pool, e.tables)
}

func exportOrderForTables(ctx context.Context, pool *pgxpool.Pool, configured []string) ([]string, error) {
	if pool == nil {
		return nil, fmt.Errorf("export order: nil pool")
	}
	if ctx == nil {
		return nil, fmt.Errorf("export order: nil context")
	}
	tables, err := normalizeTables(configured)
	if err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return []string{}, nil
	}

	present, err := listPresentTables(ctx, pool, tables)
	if err != nil {
		return nil, err
	}
	missing := make([]string, 0)
	for _, table := range tables {
		if !present[table] {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, &MissingTablesError{Tables: missing}
	}

	edges, err := listForeignKeyEdges(ctx, pool, tables)
	if err != nil {
		return nil, err
	}
	return topologicalOrder(tables, edges)
}

func normalizeTables(configured []string) ([]string, error) {
	seen := make(map[string]struct{}, len(configured))
	tables := make([]string, 0, len(configured))
	for _, table := range configured {
		if !validIdentifier(table) {
			return nil, fmt.Errorf("invalid PostgreSQL table identifier %q", table)
		}
		if _, exists := seen[table]; exists {
			return nil, fmt.Errorf("duplicate PostgreSQL table %q", table)
		}
		seen[table] = struct{}{}
		tables = append(tables, table)
	}
	return tables, nil
}

func validIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || char == '_' || (index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func listPresentTables(ctx context.Context, pool *pgxpool.Pool, tables []string) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `
		select table_name
		from information_schema.tables
		where table_schema = current_schema()
		  and table_type = 'BASE TABLE'
		  and table_name = any($1::text[])
	`, tables)
	if err != nil {
		return nil, fmt.Errorf("list configured tables: %w", err)
	}
	defer rows.Close()

	present := make(map[string]bool, len(tables))
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("scan configured table: %w", err)
		}
		present[table] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read configured tables: %w", err)
	}
	return present, nil
}

type tableEdge struct {
	parent string
	child  string
}

func listForeignKeyEdges(ctx context.Context, pool *pgxpool.Pool, tables []string) ([]tableEdge, error) {
	rows, err := pool.Query(ctx, `
		select distinct
			tc.table_name as referencing_table,
			ccu.table_name as referenced_table
		from information_schema.table_constraints tc
		join information_schema.key_column_usage kcu
		  on kcu.constraint_schema = tc.constraint_schema
		 and kcu.constraint_name = tc.constraint_name
		 and kcu.table_name = tc.table_name
		join information_schema.referential_constraints rc
		  on rc.constraint_schema = tc.constraint_schema
		 and rc.constraint_name = tc.constraint_name
		join information_schema.constraint_column_usage ccu
		  on ccu.constraint_schema = rc.unique_constraint_schema
		 and ccu.constraint_name = rc.unique_constraint_name
		where tc.constraint_type = 'FOREIGN KEY'
		  and tc.table_schema = current_schema()
		  and ccu.table_schema = current_schema()
		  and tc.table_name = any($1::text[])
		  and ccu.table_name = any($1::text[])
	`, tables)
	if err != nil {
		return nil, fmt.Errorf("list configured foreign keys: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	edges := make([]tableEdge, 0)
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, fmt.Errorf("scan configured foreign key: %w", err)
		}
		key := parent + "\x00" + child
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		edges = append(edges, tableEdge{parent: parent, child: child})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read configured foreign keys: %w", err)
	}
	return edges, nil
}

func topologicalOrder(tables []string, edges []tableEdge) ([]string, error) {
	indegree := make(map[string]int, len(tables))
	adjacency := make(map[string][]string, len(tables))
	for _, table := range tables {
		indegree[table] = 0
		adjacency[table] = nil
	}
	seen := make(map[string]struct{}, len(edges))
	for _, edge := range edges {
		if _, ok := indegree[edge.parent]; !ok {
			continue
		}
		if _, ok := indegree[edge.child]; !ok {
			continue
		}
		key := edge.parent + "\x00" + edge.child
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		adjacency[edge.parent] = append(adjacency[edge.parent], edge.child)
		indegree[edge.child]++
	}
	for table := range adjacency {
		sort.Strings(adjacency[table])
	}

	ready := make([]string, 0, len(tables))
	for _, table := range tables {
		if indegree[table] == 0 {
			ready = append(ready, table)
		}
	}
	sort.Strings(ready)
	result := make([]string, 0, len(tables))
	for len(ready) > 0 {
		table := ready[0]
		ready = ready[1:]
		result = append(result, table)
		for _, child := range adjacency[table] {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
			}
		}
		sort.Strings(ready)
	}
	if len(result) == len(tables) {
		return result, nil
	}

	cycle := findCycle(tables, adjacency, indegree)
	return nil, &ForeignKeyCycleError{Path: cycle}
}

func findCycle(tables []string, adjacency map[string][]string, indegree map[string]int) []string {
	state := make(map[string]uint8, len(tables))
	stack := make([]string, 0, len(tables))
	positions := make(map[string]int, len(tables))
	var visit func(string) []string
	visit = func(table string) []string {
		state[table] = 1
		positions[table] = len(stack)
		stack = append(stack, table)
		for _, next := range adjacency[table] {
			if indegree[next] == 0 {
				continue
			}
			switch state[next] {
			case 0:
				if cycle := visit(next); len(cycle) > 0 {
					return cycle
				}
			case 1:
				cycle := append([]string(nil), stack[positions[next]:]...)
				cycle = append(cycle, next)
				return cycle
			}
		}
		stack = stack[:len(stack)-1]
		delete(positions, table)
		state[table] = 2
		return nil
	}

	sorted := append([]string(nil), tables...)
	sort.Strings(sorted)
	for _, table := range sorted {
		if indegree[table] > 0 && state[table] == 0 {
			if cycle := visit(table); len(cycle) > 0 {
				return cycle
			}
		}
	}
	return sorted
}
