package engine

// graph_store.go — persisted, versioned graphs (loom_graphs) and the registry
// that saves, validates and plans them.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
)

// GraphRecord is a saved, versioned graph. Records never change: an edit is a
// new version, and SetActive chooses which version serves.
type GraphRecord struct {
	ID        uuid.UUID
	Owner     string // opaque app-owned scope; "" = global
	Slug      string
	Version   int
	Category  string
	IsActive  bool
	Graph     Graph // Graph.Slug is set from Slug on save
	CreatedAt time.Time
}

// GraphRegistry saves, resolves, validates and plans graphs, with the same
// version semantics as FlowRegistry: Get with version 0 resolves the latest
// ACTIVE version, Latest includes drafts, SetActive is publish and rollback.
type GraphRegistry interface {
	// Create saves a new version. It refuses a graph that cannot run (the
	// checks RunGraph makes) and one that carries per-request data: Overrides
	// hold API keys and belong on GraphRequest, never in the database.
	Create(ctx context.Context, r *GraphRecord) error
	Get(ctx context.Context, owner, slug string, version int) (*GraphRecord, error)
	Latest(ctx context.Context, owner, slug string) (*GraphRecord, error)
	LatestActive(ctx context.Context, owner, slug string) (*GraphRecord, error)
	SetActive(ctx context.Context, owner, slug string, version int) error
	// List returns an owner's graphs (optional category filter), every version.
	List(ctx context.Context, owner, category string) ([]*GraphRecord, error)
	Delete(ctx context.Context, owner, slug string, version int) error
	// Validate runs RunGraph's checks plus the ones that need the registry:
	// every agent exists in the owner, every input a node consumes is produced
	// or declared, and no two nodes that can run in parallel Save the same var
	// without a reducer. It returns nil or an error wrapping ErrInvalidGraph
	// that joins every problem found.
	Validate(ctx context.Context, owner string, g Graph) error
	// Plan returns the graph as data — nodes, edges (inferred data edges
	// marked), problems and an estimated cost per agent node — without
	// refusing a broken graph. It is what a visual editor draws.
	Plan(ctx context.Context, owner string, g Graph) (*GraphPlan, error)
}

// GraphPlan is a graph's resolved wiring, for inspection and visualization.
type GraphPlan struct {
	Nodes    []GraphPlanNode
	Edges    []GraphPlanEdge
	Problems []string // empty for a valid graph
}

// GraphPlanNode is one node in a plan.
type GraphPlanNode struct {
	ID        string
	Kind      GraphNodeKind
	Agent     string // agent slug, for an agent node
	Func      string // function name, for a func node
	OutputKey string
	Consumes  []string // inputs its user template declares, for an agent node
	// EstimatedUSD is the agent's mean cost per step over the last 30 days, 0
	// when it has no history or is not an agent node. Priced is false when that
	// history includes steps priced from the placeholder rate.
	EstimatedUSD float64
	Priced       bool
}

// GraphPlanEdge is one edge in a plan.
type GraphPlanEdge struct {
	From, To string
	// Data marks an edge inferred from a template variable (Var); data edges
	// order nodes but never start them.
	Data bool
	Var  string
	// Trigger describes a control edge: "", "route=<v>", "when: <expr>",
	// "uncertain<p", "default" or "error".
	Trigger string
}

type graphService struct{ e *Engine }

// Graphs returns the saved-graph registry.
func (e *Engine) Graphs() GraphRegistry { return e.graphs }

func (s *graphService) Create(ctx context.Context, r *GraphRecord) error {
	if r.Version < 1 {
		return fmt.Errorf("noewright: graph %q: version must be >= 1 (0 is reserved for latest resolution)", r.Slug)
	}
	if r.Slug == "" {
		return fmt.Errorf("noewright: graph needs a slug")
	}
	r.Graph.Slug = r.Slug
	for _, n := range r.Graph.Nodes {
		if n.Agent != nil && len(n.Agent.Overrides) > 0 {
			return fmt.Errorf("%w: node %q sets Agent.Overrides, which hold per-request secrets; pass them on GraphRequest.Overrides instead", ErrInvalidGraph, n.ID)
		}
	}
	if _, err := s.e.compileGraph(ctx, r.Owner, r.Graph); err != nil {
		return err
	}
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	def, err := json.Marshal(r.Graph)
	if err != nil {
		return fmt.Errorf("noewright: graph %q: encode: %w", r.Slug, err)
	}
	if _, err := s.e.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sgraphs (id, owner, slug, version, category, is_active, definition, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, s.e.prefix),
		r.ID, r.Owner, r.Slug, r.Version, r.Category, flowBool(r.IsActive), string(def), r.CreatedAt,
	); err != nil {
		return err
	}
	cacheDelete(ctx, s.e.cache, s.e.cacheKeyOwnedLatest("graph-active", r.Owner, r.Slug))
	return nil
}

const graphColumns = `id, owner, slug, version, category, is_active, definition, created_at`

func (s *graphService) query(ctx context.Context, where string, args ...any) (*GraphRecord, error) {
	row := s.e.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %sgraphs WHERE %s`, graphColumns, s.e.prefix, where), args...)
	return scanGraph(row)
}

func (s *graphService) Get(ctx context.Context, owner, slug string, version int) (*GraphRecord, error) {
	if version == 0 {
		return s.LatestActive(ctx, owner, slug)
	}
	r, err := s.query(ctx, `owner=$1 AND slug=$2 AND version=$3`, owner, slug, version)
	return r, graphNotFound(err, slug, version)
}

func (s *graphService) Latest(ctx context.Context, owner, slug string) (*GraphRecord, error) {
	r, err := s.query(ctx, `owner=$1 AND slug=$2 ORDER BY version DESC LIMIT 1`, owner, slug)
	return r, graphNotFound(err, slug, 0)
}

func (s *graphService) LatestActive(ctx context.Context, owner, slug string) (*GraphRecord, error) {
	key := s.e.cacheKeyOwnedLatest("graph-active", owner, slug)
	return cachedLatest(ctx, s.e, key, func() (*GraphRecord, error) {
		r, err := s.query(ctx, `owner=$1 AND slug=$2 AND is_active=1 ORDER BY version DESC LIMIT 1`, owner, slug)
		return r, graphNotFound(err, slug, 0)
	})
}

func (s *graphService) SetActive(ctx context.Context, owner, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("noewright: set active graph %q: an explicit version >= 1 is required", slug)
	}
	tx, err := s.e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Activate the target first, so a missing version leaves the current
	// pointer untouched.
	res, err := tx.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %sgraphs SET is_active=1 WHERE owner=$1 AND slug=$2 AND version=$3`, s.e.prefix), owner, slug, version)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return &NotFoundError{Kind: "graph", Key: fmt.Sprintf("%s@v%d", slug, version)}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %sgraphs SET is_active=0 WHERE owner=$1 AND slug=$2 AND version<>$3`, s.e.prefix), owner, slug, version); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	cacheDelete(ctx, s.e.cache, s.e.cacheKeyOwnedLatest("graph-active", owner, slug))
	return nil
}

func (s *graphService) List(ctx context.Context, owner, category string) ([]*GraphRecord, error) {
	q := fmt.Sprintf(`SELECT %s FROM %sgraphs WHERE owner=$1`, graphColumns, s.e.prefix)
	args := []any{owner}
	if category != "" {
		q += " AND category=$2"
		args = append(args, category)
	}
	rows, err := s.e.db.QueryContext(ctx, q+" ORDER BY slug, version", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GraphRecord
	for rows.Next() {
		r, err := scanGraph(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *graphService) Delete(ctx context.Context, owner, slug string, version int) error {
	if version < 1 {
		return fmt.Errorf("noewright: delete graph %q: an explicit version >= 1 is required", slug)
	}
	if _, err := s.e.db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %sgraphs WHERE owner=$1 AND slug=$2 AND version=$3`, s.e.prefix), owner, slug, version); err != nil {
		return err
	}
	cacheDelete(ctx, s.e.cache, s.e.cacheKeyOwnedLatest("graph-active", owner, slug))
	return nil
}

func scanGraph(row interface{ Scan(...any) error }) (*GraphRecord, error) {
	var (
		r        GraphRecord
		isActive int
		def      string
	)
	err := row.Scan(&r.ID, &r.Owner, &r.Slug, &r.Version, &r.Category, &isActive, &def, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.IsActive = isActive != 0
	if err := json.Unmarshal([]byte(def), &r.Graph); err != nil {
		return nil, fmt.Errorf("noewright: graph %q v%d: decode: %w", r.Slug, r.Version, err)
	}
	return &r, nil
}

func graphNotFound(err error, slug string, version int) error {
	if errors.Is(err, ErrNotFound) {
		key := slug
		if version > 0 {
			key = fmt.Sprintf("%s@v%d", slug, version)
		}
		return &NotFoundError{Kind: "graph", Key: key}
	}
	return err
}

// RunGraphBySlug loads a saved graph (version 0 = the latest active version)
// within owner and runs it. req.Graph is ignored; req.Owner is set to owner.
func (e *Engine) RunGraphBySlug(ctx context.Context, session *Session, owner, slug string, version int, req GraphRequest) (*GraphRun, error) {
	rec, err := e.graphs.Get(ctx, owner, slug, version)
	if err != nil {
		return nil, fmt.Errorf("noewright: run graph %q: %w", slug, err)
	}
	req.Graph = rec.Graph
	req.Owner = owner
	return e.RunGraph(ctx, session, req)
}

func (s *graphService) Validate(ctx context.Context, owner string, g Graph) error {
	c, err := s.e.compileGraph(ctx, owner, g)
	if err != nil {
		return err
	}
	if probs := s.problems(ctx, owner, c); len(probs) > 0 {
		errs := make([]error, len(probs))
		for i, p := range probs {
			errs[i] = errors.New(p)
		}
		return fmt.Errorf("%w: %w", ErrInvalidGraph, errors.Join(errs...))
	}
	return nil
}

// problems runs the checks that need the registry, on a compiled graph.
func (s *graphService) problems(ctx context.Context, owner string, c *compiledGraph) []string {
	var probs []string
	g := c.g
	produced := map[string]bool{"action": true}
	for _, in := range g.Inputs {
		produced[in] = true
	}
	for _, k := range c.output {
		if k != "" {
			produced[k] = true
		}
	}
	for _, n := range g.Nodes {
		if n.Kind != NodeAgent {
			continue
		}
		if _, err := s.e.agents.Get(ctx, owner, n.Agent.AgentSlug, n.Agent.AgentVersion); err != nil {
			probs = append(probs, fmt.Sprintf("node %q: agent %q: %v", n.ID, n.Agent.AgentSlug, err))
			continue
		}
		for _, v := range s.e.userTemplateVars(ctx, owner, n.Agent.AgentSlug, n.Agent.AgentVersion) {
			if _, own := n.Agent.Inputs[v]; !produced[v] && !own {
				probs = append(probs, fmt.Sprintf("node %q reads %q, which no node produces and the graph does not declare as an input", n.ID, v))
			}
		}
	}

	// Two nodes that can run at the same time must not both Save a var that
	// has no reducer: which write wins would depend on timing.
	reach := c.reachability()
	savers := map[string][]int{}
	for i, n := range g.Nodes {
		for v := range n.Save {
			if r := g.Vars[v].Reducer; r == "" || r == ReduceReplace {
				savers[v] = append(savers[v], i)
			}
		}
	}
	vars := make([]string, 0, len(savers))
	for v := range savers {
		vars = append(vars, v)
	}
	sort.Strings(vars)
	for _, v := range vars {
		ids := savers[v]
		for a := 0; a < len(ids); a++ {
			for b := a + 1; b < len(ids); b++ {
				x, y := ids[a], ids[b]
				if !reach[x][y] && !reach[y][x] {
					probs = append(probs, fmt.Sprintf("nodes %q and %q can run in parallel and both save %q; declare a reducer for it in Vars",
						g.Nodes[x].ID, g.Nodes[y].ID, v))
				}
			}
		}
	}
	return probs
}

// reachability returns reach[i][j] = j can only run after i (a path of
// control or data edges leads from i to j).
func (c *compiledGraph) reachability() [][]bool {
	n := len(c.g.Nodes)
	reach := make([][]bool, n)
	for i := range n {
		reach[i] = make([]bool, n)
		stack := []int{i}
		for len(stack) > 0 {
			k := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			var next []int
			for _, ei := range c.out[k] {
				next = append(next, c.edges[ei].to...)
			}
			next = append(next, c.data[k]...)
			for _, j := range next {
				if !reach[i][j] {
					reach[i][j] = true
					stack = append(stack, j)
				}
			}
		}
	}
	return reach
}

func (s *graphService) Plan(ctx context.Context, owner string, g Graph) (*GraphPlan, error) {
	plan := &GraphPlan{}
	c, err := s.e.compileGraph(ctx, owner, g)
	if err != nil {
		// A graph that does not compile still gets drawn, from what was
		// written, with the reason it cannot run.
		plan.Problems = append(plan.Problems, err.Error())
		for _, n := range g.Nodes {
			plan.Nodes = append(plan.Nodes, graphPlanNode(n))
		}
		for _, ed := range g.Edges {
			for _, t := range ed.To {
				plan.Edges = append(plan.Edges, GraphPlanEdge{From: ed.From, To: t, Trigger: edgeTrigger(ed)})
			}
		}
		return plan, nil
	}
	plan.Problems = s.problems(ctx, owner, c)
	for _, n := range g.Nodes {
		pn := graphPlanNode(n)
		if n.Kind == NodeAgent {
			pn.Consumes = s.e.userTemplateVars(ctx, owner, n.Agent.AgentSlug, n.Agent.AgentVersion)
			if a, err := s.e.agents.Get(ctx, owner, n.Agent.AgentSlug, n.Agent.AgentVersion); err == nil {
				if st, err := s.e.costs.AgentStats(ctx, a.ID, 30*24*time.Hour); err == nil && st != nil {
					pn.EstimatedUSD, pn.Priced = st.USDMean, st.PricingConfigured
				}
			}
		}
		plan.Nodes = append(plan.Nodes, pn)
	}
	for _, ce := range c.edges {
		for _, t := range ce.to {
			plan.Edges = append(plan.Edges, GraphPlanEdge{From: ce.From, To: g.Nodes[t].ID, Trigger: edgeTrigger(ce.GraphEdge)})
		}
	}
	for p, consumers := range c.data {
		for _, t := range consumers {
			key := c.output[p]
			if !slices.Contains(plan.Nodes[t].Consumes, key) {
				continue
			}
			plan.Edges = append(plan.Edges, GraphPlanEdge{From: g.Nodes[p].ID, To: g.Nodes[t].ID, Data: true, Var: key})
		}
	}
	return plan, nil
}

func graphPlanNode(n GraphNode) GraphPlanNode {
	pn := GraphPlanNode{ID: n.ID, Kind: n.Kind, Func: n.Func, OutputKey: n.OutputKey}
	if n.Agent != nil {
		pn.Agent, pn.OutputKey = n.Agent.AgentSlug, n.Agent.OutputKey
	}
	return pn
}

func edgeTrigger(ed GraphEdge) string {
	switch {
	case ed.Route != "":
		return "route=" + ed.Route
	case ed.When != "":
		return "when: " + ed.When
	case ed.Uncertain != 0:
		return fmt.Sprintf("uncertain<%g", ed.Uncertain)
	case ed.Default:
		return "default"
	case ed.OnError:
		return "error"
	}
	return ""
}
