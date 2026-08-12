package engine

import (
	"fmt"

	"octopus/internal/domain"
)

// Levels groups a PipelineDef's node IDs into levels: every node in a level
// has all its dependencies satisfied by strictly earlier levels, and nodes
// within the same level have no dependency relationship to each other —
// they're exactly the set the engine is free to run concurrently.
func Levels(def *domain.PipelineDef) ([][]string, error) {
	nodeByID := make(map[string]domain.NodeDef, len(def.Nodes))
	for _, n := range def.Nodes {
		if _, exists := nodeByID[n.ID]; exists {
			return nil, fmt.Errorf("duplicate node id %q", n.ID)
		}
		nodeByID[n.ID] = n
	}

	depCount := make(map[string]int, len(def.Nodes))
	dependents := make(map[string][]string, len(def.Nodes))

	for _, e := range def.Edges {
		if _, ok := nodeByID[e.From]; !ok {
			return nil, fmt.Errorf("edge references unknown node %q", e.From)
		}
		if _, ok := nodeByID[e.To]; !ok {
			return nil, fmt.Errorf("edge references unknown node %q", e.To)
		}
		depCount[e.To]++
		dependents[e.From] = append(dependents[e.From], e.To)
	}

	var levels [][]string
	done := make(map[string]bool, len(def.Nodes))

	for len(done) < len(def.Nodes) {
		var level []string
		for _, n := range def.Nodes {
			if done[n.ID] {
				continue
			}
			if depCount[n.ID] == 0 {
				level = append(level, n.ID)
			}
		}
		if len(level) == 0 {
			return nil, fmt.Errorf("cycle detected in pipeline definition %q", def.ID)
		}
		for _, id := range level {
			done[id] = true
			for _, dep := range dependents[id] {
				depCount[dep]--
			}
		}
		levels = append(levels, level)
	}

	return levels, nil
}
