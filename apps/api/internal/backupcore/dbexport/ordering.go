package dbexport

import (
	"fmt"
	"sort"
	"strings"
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

type tableEdge struct {
	parent string
	child  string
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

// SQLiteExportOrder is the fixed export order for the SQLite exporter. It is
// the deterministic topological order (see topologicalOrder) of
// DefaultConfigTables over the foreign keys of the migrated SQLite schema; a
// test recomputes it from PRAGMA foreign_key_list so it cannot drift.
var SQLiteExportOrder = []string{
	"cluster_keys",
	"installation_identity",
	"installation_settings",
	"internal_ca_enrollment_state",
	"node_state",
	"resolver_pools",
	"secrets",
	"stream_routes",
	"users",
	"zones",
	"forwarding_rules",
	"certificates",
	"internal_ca",
	"provider_connections",
	"config_revisions",
	"dns_records",
}
