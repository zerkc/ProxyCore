package dbexport

import (
	"errors"
	"testing"
)

func TestTopologicalOrderReportsCyclePath(t *testing.T) {
	_, err := topologicalOrder(
		[]string{"a", "b"},
		[]tableEdge{{parent: "a", child: "b"}, {parent: "b", child: "a"}},
	)
	if err == nil {
		t.Fatal("topologicalOrder accepted a cycle")
	}
	var cycle *ForeignKeyCycleError
	if !errors.As(err, &cycle) {
		t.Fatalf("error = %v, want ForeignKeyCycleError", err)
	}
	if len(cycle.Path) < 3 || cycle.Path[0] != cycle.Path[len(cycle.Path)-1] {
		t.Fatalf("cycle path = %v, want closed path", cycle.Path)
	}
}
