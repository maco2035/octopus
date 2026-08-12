package domain

// NodeDef is one node in a pipeline's DAG, as built in the drag-and-drop
// web UI (Phase 4) and looked up against the agent registry at run time.
type NodeDef struct {
	ID             string
	AgentType      string
	Config         map[string]any
	RequiresReview bool // if true, the engine pauses after this node until a human continues it
}

type EdgeDef struct {
	From string // NodeDef.ID
	To   string // NodeDef.ID
}

type PipelineDef struct {
	ID        string
	ProjectID string
	Name      string
	Nodes     []NodeDef
	Edges     []EdgeDef
}
