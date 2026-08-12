package engine_test

import (
	"testing"

	"octopus/internal/domain"
	"octopus/internal/engine"
)

func TestLevels_DetectsCycle(t *testing.T) {
	def := &domain.PipelineDef{
		ID: "cyclic",
		Nodes: []domain.NodeDef{
			{ID: "A"},
			{ID: "B"},
		},
		Edges: []domain.EdgeDef{
			{From: "A", To: "B"},
			{From: "B", To: "A"},
		},
	}

	if _, err := engine.Levels(def); err == nil {
		t.Fatal("expected an error for a cyclic pipeline definition, got nil")
	}
}

func TestLevels_DiamondShape(t *testing.T) {
	def := &domain.PipelineDef{
		ID: "diamond",
		Nodes: []domain.NodeDef{
			{ID: "A"}, {ID: "B"}, {ID: "C"}, {ID: "D"},
		},
		Edges: []domain.EdgeDef{
			{From: "A", To: "B"},
			{From: "A", To: "C"},
			{From: "B", To: "D"},
			{From: "C", To: "D"},
		},
	}

	levels, err := engine.Levels(def)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(levels) != 3 {
		t.Fatalf("expected 3 levels, got %d: %v", len(levels), levels)
	}
	if len(levels[0]) != 1 || levels[0][0] != "A" {
		t.Fatalf("expected level 0 = [A], got %v", levels[0])
	}
	if len(levels[1]) != 2 {
		t.Fatalf("expected level 1 to have 2 nodes (B, C), got %v", levels[1])
	}
	if len(levels[2]) != 1 || levels[2][0] != "D" {
		t.Fatalf("expected level 2 = [D], got %v", levels[2])
	}
}
