package engine

// graph_run.go — RunGraph: compiles a Graph onto the shared executor.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/google/uuid"
)

// ErrInvalidGraph is returned when a graph cannot run as written. The wrapped
// error names the problem.
var ErrInvalidGraph = errors.New("noewright: invalid graph")

// ErrGraphMismatch is returned when RunGraph is called for one graph on a
// session paused in another. Set GraphRequest.Restart to abandon the pause.
var ErrGraphMismatch = errors.New("noewright: session is paused in a different graph")

// ErrWaitInput is returned when a paused run is resumed without an Action, or
// with one its wait node does not accept. Nothing changes: the session stays
// paused at the same node, so the caller can ask again.
var ErrWaitInput = errors.New("noewright: input not accepted by the wait node")

// RegisterFunc registers a Go function that NodeFunc nodes call by name.
// Registering a name again replaces the function.
func (e *Engine) RegisterFunc(name string, fn GraphFunc) {
	e.funcsMu.Lock()
	defer e.funcsMu.Unlock()
	if e.funcs == nil {
		e.funcs = map[string]GraphFunc{}
	}
	e.funcs[name] = fn
}

func (e *Engine) lookupFunc(name string) (GraphFunc, bool) {
	e.funcsMu.RLock()
	defer e.funcsMu.RUnlock()
	fn, ok := e.funcs[name]
	return fn, ok
}

// exprShape is the expression environment's shape, used to compile and
// type-check expressions: an unknown top-level name is a compile error.
func exprShape() map[string]any {
	return map[string]any{
		"inputs": map[string]any{},
		"data":   map[string]any{},
		"vars":   map[string]any{},
		"eval":   map[string]any{},
		"run":    map[string]any{},
		"visits": map[string]any{},
	}
}

func compileCond(src string) (*vm.Program, error) {
	return expr.Compile(src, expr.Env(exprShape()), expr.AsBool())
}

func compileValue(src string) (*vm.Program, error) {
	return expr.Compile(src, expr.Env(exprShape()))
}

// compiledEdge is a GraphEdge with its condition compiled and its targets
// resolved to node indexes.
type compiledEdge struct {
	GraphEdge
	from int
	to   []int
	when *vm.Program
}

// compiledGraph is a graph checked and indexed for running.
type compiledGraph struct {
	g      Graph
	index  map[string]int // node id → index
	out    [][]int        // control edges leaving each node (indexes into edges)
	edges  []compiledEdge
	data   [][]int // data-edge targets per node (inferred from templates)
	saves  []map[string]*vm.Program
	output []string // each node's output key
	wait   []bool   // wait nodes: edges into them pause the run
	match  []*regexp.Regexp
}

// compileGraph checks a graph's structure and compiles its expressions. It is
// the check RunGraph makes before running; Graphs().Validate adds registry
// checks on top.
func (e *Engine) compileGraph(ctx context.Context, owner string, g Graph) (*compiledGraph, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidGraph, fmt.Sprintf(format, args...))
	}
	c := &compiledGraph{g: g, index: map[string]int{}}
	n := len(g.Nodes)
	if n == 0 {
		return nil, bad("graph %q has no nodes", g.Slug)
	}
	c.out = make([][]int, n)
	c.data = make([][]int, n)
	c.saves = make([]map[string]*vm.Program, n)
	c.output = make([]string, n)
	c.wait = make([]bool, n)
	c.match = make([]*regexp.Regexp, n)
	producer := map[string]int{}
	for i, nd := range g.Nodes {
		if nd.ID == "" {
			return nil, bad("node %d has no id", i)
		}
		if _, dup := c.index[nd.ID]; dup {
			return nil, bad("node id %q is used twice", nd.ID)
		}
		c.index[nd.ID] = i
		switch nd.Kind {
		case NodeAgent:
			if nd.Agent == nil || nd.Agent.AgentSlug == "" {
				return nil, bad("agent node %q names no agent", nd.ID)
			}
			if nd.Agent.When != nil {
				return nil, bad("agent node %q sets Agent.When; route with edge conditions instead", nd.ID)
			}
			c.output[i] = nd.Agent.OutputKey
		case NodeFunc:
			if nd.Func == "" {
				return nil, bad("func node %q names no function", nd.ID)
			}
			if _, ok := e.lookupFunc(nd.Func); !ok {
				return nil, bad("func node %q: no function registered as %q", nd.ID, nd.Func)
			}
			c.output[i] = nd.OutputKey
		case NodeDecide:
			if nd.Decide == nil {
				return nil, bad("decide node %q has no question", nd.ID)
			}
			if err := nd.Decide.Question.Validate(); err != nil {
				return nil, bad("decide node %q: %v", nd.ID, err)
			}
			c.output[i] = nd.OutputKey
		case NodeWait:
			c.wait[i] = true
			c.output[i] = nd.OutputKey
			if w := nd.Wait; w != nil {
				switch w.Expect {
				case "", ExpectAny, ExpectText:
				case ExpectChoice:
					if len(w.Options) == 0 {
						return nil, bad("wait node %q expects a choice but lists no options", nd.ID)
					}
				default:
					return nil, bad("wait node %q has unknown expect %q", nd.ID, w.Expect)
				}
				if w.Pattern != "" {
					re, err := regexp.Compile(w.Pattern)
					if err != nil {
						return nil, bad("wait node %q pattern: %v", nd.ID, err)
					}
					c.match[i] = re
				}
			}
		default:
			return nil, bad("node %q has unknown kind %q", nd.ID, nd.Kind)
		}
		if k := c.output[i]; k != "" {
			if p, dup := producer[k]; dup {
				return nil, bad("output %q is produced by both %q and %q", k, g.Nodes[p].ID, nd.ID)
			}
			producer[k] = i
		}
		for v, src := range nd.Save {
			p, err := compileValue(src)
			if err != nil {
				return nil, bad("node %q save %q: %v", nd.ID, v, err)
			}
			if c.saves[i] == nil {
				c.saves[i] = map[string]*vm.Program{}
			}
			c.saves[i][v] = p
		}
	}
	entry, ok := c.index[g.Entry]
	if !ok {
		return nil, bad("entry %q is not a node", g.Entry)
	}
	if c.wait[entry] {
		return nil, bad("entry %q is a wait node; start the graph with the node that runs first", g.Entry)
	}

	hasIn := make([]bool, n)
	for k, ed := range g.Edges {
		from, ok := c.index[ed.From]
		if !ok {
			return nil, bad("edge %d leaves %q, which is not a node", k, ed.From)
		}
		if len(ed.To) == 0 {
			return nil, bad("edge %d from %q leads nowhere", k, ed.From)
		}
		triggers := 0
		for _, set := range []bool{ed.Route != "", ed.When != "", ed.Uncertain != 0, ed.Default, ed.OnError} {
			if set {
				triggers++
			}
		}
		if triggers > 1 {
			return nil, bad("edge %d from %q sets more than one of route, when, uncertain, default, on error", k, ed.From)
		}
		kind := g.Nodes[from].Kind
		if ed.Uncertain != 0 && kind != NodeDecide {
			return nil, bad("edge %d: uncertain is only for edges from a decide node, %q is %s", k, ed.From, kind)
		}
		if ed.Uncertain < 0 || ed.Uncertain > 1 {
			return nil, bad("edge %d: uncertain must be between 0 and 1", k)
		}
		if ed.Route != "" && kind == NodeAgent {
			return nil, bad("edge %d: route needs a decide, func or wait node, %q is an agent", k, ed.From)
		}
		ce := compiledEdge{GraphEdge: ed, from: from}
		for _, t := range ed.To {
			to, ok := c.index[t]
			if !ok {
				return nil, bad("edge %d from %q leads to %q, which is not a node", k, ed.From, t)
			}
			ce.to = append(ce.to, to)
			hasIn[to] = true
		}
		if ed.When != "" {
			p, err := compileCond(ed.When)
			if err != nil {
				return nil, bad("edge %d from %q: when: %v", k, ed.From, err)
			}
			ce.when = p
		}
		c.out[from] = append(c.out[from], len(c.edges))
		c.edges = append(c.edges, ce)
	}
	for i := range g.Nodes {
		if i != entry && !hasIn[i] {
			return nil, bad("node %q has no edge into it", g.Nodes[i].ID)
		}
	}

	// Data edges: a node whose template declares another node's output runs
	// after that producer.
	for i, nd := range g.Nodes {
		if nd.Kind != NodeAgent {
			continue
		}
		for _, v := range e.userTemplateVars(ctx, owner, nd.Agent.AgentSlug, nd.Agent.AgentVersion) {
			if p, ok := producer[v]; ok && p != i && !slices.Contains(c.data[p], i) {
				c.data[p] = append(c.data[p], i)
			}
		}
	}

	// A cycle must pass through a wait node: edges into a wait pause the run
	// rather than start the node, so only those break a loop.
	if cyc := c.cycle(); cyc != "" {
		return nil, bad("nodes form a cycle through %q with no wait node in it", cyc)
	}
	return c, nil
}

// cycle returns a node on a cycle of control or data edges, or "".
func (c *compiledGraph) cycle() string {
	n := len(c.g.Nodes)
	state := make([]int, n) // 0 unvisited, 1 on stack, 2 done
	var visit func(i int) int
	visit = func(i int) int {
		state[i] = 1
		var next []int
		for _, k := range c.out[i] {
			for _, t := range c.edges[k].to {
				if !c.wait[t] {
					next = append(next, t)
				}
			}
		}
		next = append(next, c.data[i]...)
		for _, j := range next {
			switch state[j] {
			case 1:
				return j
			case 0:
				if r := visit(j); r >= 0 {
					return r
				}
			}
		}
		state[i] = 2
		return -1
	}
	for i := range n {
		if state[i] == 0 {
			if r := visit(i); r >= 0 {
				return c.g.Nodes[r].ID
			}
		}
	}
	return ""
}

// graphRunState is what a run accumulates beside the executor's own result.
// Node goroutines write it under mu; the coordinator reads it under mu.
type graphRunState struct {
	mu       sync.Mutex
	data     map[string]any // output key → structured data
	evals    map[string]any // decide node id → answer summary
	funcVars map[int]map[string]any
	path     []GraphHop
	edgeErrs map[string]error
	failed   map[string]any // node id → error message, for run.errors
	changed  bool           // session vars were written by the run
	visits   map[string]int // node runs in this session's use of the graph
	pauses   []int          // wait nodes an edge fired into, in firing order
}

// graphLock serialises RunGraph calls on one session within this engine.
func (e *Engine) graphLock(id uuid.UUID) *sync.Mutex {
	l, _ := e.graphLocks.LoadOrStore(id, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// waitInput checks an action against a wait node and returns the node's output.
func waitInput(w *GraphWait, re *regexp.Regexp, a *Action) (string, error) {
	if a == nil {
		return "", fmt.Errorf("%w: resuming needs an Action", ErrWaitInput)
	}
	text := actionText(a)
	expect := ExpectAny
	if w != nil && w.Expect != "" {
		expect = w.Expect
	}
	switch expect {
	case ExpectText:
		if text == "" {
			return "", fmt.Errorf("%w: expects text", ErrWaitInput)
		}
		if re != nil && !re.MatchString(text) {
			return "", fmt.Errorf("%w: %q does not match %s", ErrWaitInput, text, re)
		}
	case ExpectChoice:
		if p, ok := a.Payload.(map[string]any); ok && a.Kind == ActionSelect {
			if id, ok := p["option_id"].(string); ok && id != "" {
				if slices.Contains(w.Options, id) {
					return id, nil
				}
				return "", fmt.Errorf("%w: %q is not an option", ErrWaitInput, id)
			}
			if raw, ok := p["option_index"]; ok {
				if i := toInt(raw); i >= 0 && i < len(w.Options) {
					return w.Options[i], nil
				}
				return "", fmt.Errorf("%w: option index %v is out of range", ErrWaitInput, raw)
			}
		}
		if slices.Contains(w.Options, text) {
			return text, nil
		}
		return "", fmt.Errorf("%w: expects one of %v", ErrWaitInput, w.Options)
	}
	return text, nil
}

// RunGraph runs a graph against a session, from its entry node until nothing
// is left to run.
//
// A non-nil error means the graph could not run (ErrInvalidGraph) or a
// Required node failed; in the second case the partial GraphRun is returned
// with it. Other node failures are recorded on GraphRun.Errors.
func (e *Engine) RunGraph(ctx context.Context, session *Session, req GraphRequest) (*GraphRun, error) {
	c, err := e.compileGraph(ctx, req.Owner, req.Graph)
	if err != nil {
		return nil, err
	}
	g := req.Graph
	lock := e.graphLock(session.ID)
	lock.Lock()
	defer lock.Unlock()

	// Where does this run start: the entry, or the wait the session is paused at?
	entry, resumeOut := c.index[g.Entry], ""
	cur := session.State.Graph
	hadCursor := cur != nil
	if cur != nil && req.Restart {
		cur = nil
	}
	if cur != nil {
		if cur.Graph != g.Slug {
			return nil, fmt.Errorf("%w: paused in %q at %q, asked to run %q", ErrGraphMismatch, cur.Graph, cur.Node, g.Slug)
		}
		w, ok := c.index[cur.Node]
		if !ok || !c.wait[w] {
			return nil, fmt.Errorf("%w: the session is paused at %q, which graph %q has no wait node for; set Restart", ErrInvalidGraph, cur.Node, g.Slug)
		}
		out, err := waitInput(g.Nodes[w].Wait, c.match[w], req.Action)
		if err != nil {
			return nil, err
		}
		entry, resumeOut = w, out
	}
	runID := uuid.New()

	inputs := map[string]any{}
	visits := map[string]int{}
	if cur != nil {
		maps.Copy(inputs, cur.Inputs)
		maps.Copy(visits, cur.Visits)
		delete(inputs, "action") // the new action replaces the one the run paused after
	}
	maps.Copy(inputs, req.Inputs)
	if _, ok := inputs["action"]; !ok {
		if t := actionText(req.Action); t != "" {
			inputs["action"] = t
		}
	}
	bus := newBus()
	defer bus.close()

	gs := &graphRunState{data: map[string]any{}, evals: map[string]any{}, funcVars: map[int]map[string]any{},
		edgeErrs: map[string]error{}, failed: map[string]any{}, visits: visits}
	nodeEvent := func(ev NodeEvent) {
		if req.OnNode != nil {
			req.OnNode(ev)
		}
	}

	eg := &execGraph{entry: entry, nodes: make([]execNode, len(g.Nodes)), succ: make([][]execEdge, len(g.Nodes))}
	for i := range g.Nodes {
		nd := g.Nodes[i]
		node := execNode{
			outputKey:     c.output[i],
			required:      nd.Required,
			fireOnFail:    nd.Continue,
			recoverPanics: true,
			onPanic: func(r any) error {
				perr := fmt.Errorf("noewright: graph %q node %q panicked: %v", g.Slug, nd.ID, r)
				e.log.Error("graph node panicked", "run_id", runID, "node", nd.ID, "err", perr)
				return perr
			},
			settled: func(out *execOut, err error) {
				ev := NodeEvent{Node: nd.ID}
				if outSucceeded(out, err) {
					ev.Kind, ev.Step, ev.Route = NodeDone, out.step, out.route
					if out.result != nil {
						ev.Text = ResultText(out.result)
					}
				} else {
					ev.Kind, ev.Err = NodeFailed, err
					if err != nil {
						gs.mu.Lock()
						gs.failed[nd.ID] = err.Error()
						gs.mu.Unlock()
					}
				}
				nodeEvent(ev)
			},
		}
		if nd.Kind == NodeAgent {
			node.inputs = nd.Agent.Inputs
		}
		node.run = e.graphNodeRun(session, req, c, gs, runID, i, entry, resumeOut, bus, nodeEvent)
		node.choose = e.graphNodeChoose(session, c, gs, i, inputs)

		// Outgoing edges, in the order choose reports them: control edges as
		// written (one per target, leaving out wait nodes — an edge into a wait
		// pauses the run instead of starting it), then inferred data edges.
		for _, k := range c.out[i] {
			for _, to := range c.edges[k].to {
				if !c.wait[to] {
					eg.succ[i] = append(eg.succ[i], execEdge{to: to})
				}
			}
		}
		for _, to := range c.data[i] {
			eg.succ[i] = append(eg.succ[i], execEdge{to: to, data: true})
		}
		eg.nodes[i] = node
	}

	res := runExecGraph(ctx, eg, inputs)

	run := &GraphRun{
		ID:      runID,
		Steps:   res.steps,
		Nodes:   map[string]*Step{},
		Outputs: inputs,
		Errors:  map[string]error{},
	}
	if entry != c.index[g.Entry] {
		run.Resumed = g.Nodes[entry].ID
	}
	for k, step := range res.steps {
		run.Nodes[g.Nodes[res.stepNodes[k]].ID] = step
	}
	for _, ce := range res.errs {
		run.Errors[g.Nodes[ce.node].ID] = ce.err
	}
	gs.mu.Lock()
	for k, err := range gs.edgeErrs {
		if _, taken := run.Errors[k]; !taken {
			run.Errors[k] = err
		}
	}
	run.Path = gs.path
	changed := gs.changed
	var pauses []int
	for _, w := range gs.pauses {
		if !slices.Contains(pauses, w) {
			pauses = append(pauses, w)
		}
	}
	gs.visits = maps.Clone(gs.visits)
	visitsNow := gs.visits
	gs.mu.Unlock()
	// Skipped lists nodes this run could have reached that no edge started —
	// not wait nodes, and not the nodes before a resumed wait.
	reach := map[int]bool{entry: true}
	for queue := []int{entry}; len(queue) > 0; queue = queue[1:] {
		for _, ed := range eg.succ[queue[0]] {
			if !reach[ed.to] {
				reach[ed.to] = true
				queue = append(queue, ed.to)
			}
		}
	}
	for _, i := range res.dead {
		if reach[i] && !c.wait[i] {
			run.Skipped = append(run.Skipped, g.Nodes[i].ID)
			nodeEvent(NodeEvent{Node: g.Nodes[i].ID, Kind: NodeSkipped})
		}
	}
	run.Messages = bus.Messages()
	if cost, err := e.runCost(ctx, res.steps); err != nil {
		e.log.Error("graph run cost", "run_id", runID, "err", err)
	} else {
		run.Cost = cost
	}

	var waitErr error
	if len(pauses) > 1 {
		waitErr = fmt.Errorf("%w: the run reached wait nodes %q and %q at once; a session can wait in one place",
			ErrInvalidGraph, g.Nodes[pauses[0]].ID, g.Nodes[pauses[1]].ID)
		pauses = pauses[:1]
	}

	// Where the session stands now: paused at a wait, or finished (no cursor).
	// The cursor is written into the session row and into the checkpoint of
	// the run's last step, so Fork at that step resumes at the wait.
	e.mu.Lock()
	var cursorChanged bool
	if res.abort == nil && len(pauses) == 1 {
		w := pauses[0]
		visitsNow[g.Nodes[w].ID]++
		session.State.Graph = &GraphCursor{Graph: g.Slug, Version: req.graphVersion, Node: g.Nodes[w].ID,
			Inputs: maps.Clone(inputs), Visits: visitsNow}
		run.Waiting = g.Nodes[w].ID
		if wt := g.Nodes[w].Wait; wt != nil {
			run.Prompt = wt.Prompt
		}
		cursorChanged = true
	} else if res.abort == nil && hadCursor {
		session.State.Graph = nil
		cursorChanged = true
	}
	var persistErr error
	if changed || cursorChanged {
		if cursorChanged && len(res.steps) > 0 {
			last := res.steps[0].Index
			for _, st := range res.steps {
				last = max(last, st.Index)
			}
			if err := sqlUpdateSnapshot(ctx, e.db, e.prefix, session.ID, last, session.State); err != nil {
				persistErr = fmt.Errorf("%w: checkpoint: %v", ErrSessionNotPersisted, err)
			}
		}
		if err := e.sessions.Update(ctx, session); err != nil {
			persistErr = fmt.Errorf("%w: %v", ErrSessionNotPersisted, err)
		}
	}
	e.mu.Unlock()

	if run.Waiting != "" {
		e.emit(ctx, SessionEvent{Type: EventGraphWait, SessionID: session.ID, StepIndex: -1, TurnID: runID, Node: run.Waiting})
	}
	e.emit(ctx, SessionEvent{Type: EventGraphDone, SessionID: session.ID, StepIndex: -1, TurnID: runID})
	if res.abort != nil {
		return run, fmt.Errorf("noewright: graph %q node %q: %w", g.Slug, g.Nodes[res.abortNode].ID, res.abort)
	}
	if waitErr != nil {
		return run, waitErr
	}
	return run, persistErr
}

// graphNodeRun builds the closure that does node i's work.
func (e *Engine) graphNodeRun(session *Session, req GraphRequest, c *compiledGraph, gs *graphRunState,
	runID uuid.UUID, i, entry int, resumeOut string, bus *busImpl, nodeEvent func(NodeEvent)) func(context.Context, map[string]any) (*execOut, error) {
	run := e.graphNodeWork(session, req, c, gs, runID, i, entry, resumeOut, bus, nodeEvent)
	nd := c.g.Nodes[i]
	return func(ctx context.Context, in map[string]any) (*execOut, error) {
		// Count the visit, and refuse one over the node's cap. A resumed wait
		// was counted when the run paused at it.
		gs.mu.Lock()
		if i != entry || !c.wait[i] {
			if nd.MaxVisits > 0 && gs.visits[nd.ID] >= nd.MaxVisits {
				gs.mu.Unlock()
				nodeEvent(NodeEvent{Node: nd.ID, Kind: NodeStarted})
				return nil, fmt.Errorf("noewright: node %q has run %d times, its MaxVisits", nd.ID, nd.MaxVisits)
			}
			gs.visits[nd.ID]++
		}
		gs.mu.Unlock()
		return run(ctx, in)
	}
}

// graphNodeWork builds the closure that does node i's own work.
func (e *Engine) graphNodeWork(session *Session, req GraphRequest, c *compiledGraph, gs *graphRunState,
	runID uuid.UUID, i, entry int, resumeOut string, bus *busImpl, nodeEvent func(NodeEvent)) func(context.Context, map[string]any) (*execOut, error) {
	nd := c.g.Nodes[i]
	role := "node:" + nd.ID
	isEntry := i == entry
	keepData := func(r Result) {
		if sr, ok := r.(*StructuredResult); ok && c.output[i] != "" {
			gs.mu.Lock()
			gs.data[c.output[i]] = sr.Data
			gs.mu.Unlock()
		}
	}

	switch nd.Kind {
	case NodeAgent:
		fa := *nd.Agent
		return func(ctx context.Context, in map[string]any) (*execOut, error) {
			nodeEvent(NodeEvent{Node: nd.ID, Kind: NodeStarted})
			var onChunk func(Chunk)
			if fa.Stream {
				onChunk = func(ch Chunk) {
					bus.Publish(fa.AgentSlug, nd.ID, ch.Content)
					if req.OnChunk != nil {
						req.OnChunk(nd.ID, ch)
					}
				}
			}
			var action *Action
			if isEntry {
				action = req.Action // recorded once, on the entry step
			}
			step, err := e.steps.run(ctx, session, StepRequest{
				AgentSlug:            fa.AgentSlug,
				AgentVersion:         fa.AgentVersion,
				Owner:                req.Owner,
				Action:               action,
				OnChunk:              onChunk,
				RetryMode:            fa.RetryMode,
				MaxRetries:           fa.MaxRetries,
				SystemPromptOverride: promptRefOf(fa.SystemPrompt),
				Inputs:               in,
				Params:               mergeParams(req.Params, fa.Params),
				Overrides:            pickOverrides(fa.Overrides, req.Overrides),
				GeneratorOverride:    fa.GeneratorOverride,
				turnID:               runID,
				turnRole:             role,
				bus:                  bus,
			})
			if stepSucceeded(step, err) {
				bus.Publish(fa.AgentSlug, nd.ID+".done", ResultText(step.Result))
				keepData(step.Result)
			}
			return stepOut(step, err), err
		}

	case NodeFunc:
		return func(ctx context.Context, in map[string]any) (*execOut, error) {
			nodeEvent(NodeEvent{Node: nd.ID, Kind: NodeStarted})
			fn, ok := e.lookupFunc(nd.Func)
			if !ok {
				return nil, fmt.Errorf("noewright: no function registered as %q", nd.Func)
			}
			e.mu.Lock()
			vars := maps.Clone(session.State.Vars)
			e.mu.Unlock()
			fr, err := fn(ctx, FuncCall{Node: nd.ID, Inputs: in, Vars: vars, Session: session})
			if err != nil {
				return nil, fmt.Errorf("noewright: function %q: %w", nd.Func, err)
			}
			var result Result = NewTextResult(fr.Text, "stop", 0, 0)
			if fr.Data != nil {
				data := maps.Clone(fr.Data)
				if _, ok := data["text"]; !ok && fr.Text != "" {
					data["text"] = fr.Text
				}
				result = NewStructuredResult(data, 0, 0)
			}
			keepData(result)
			if len(fr.Vars) > 0 {
				gs.mu.Lock()
				gs.funcVars[i] = fr.Vars
				gs.mu.Unlock()
			}
			out := &execOut{result: result, route: fr.Route}
			if nd.Record != "" {
				step, err := e.steps.run(ctx, session, StepRequest{
					external:  result,
					author:    "func:" + nd.Func,
					AgentSlug: nd.Record,
					Owner:     req.Owner,
					Inputs:    in,
					turnID:    runID,
					turnRole:  role,
					bus:       bus,
				})
				if !stepSucceeded(step, err) {
					return nil, err
				}
				out.step = step
				return out, err
			}
			return out, nil
		}

	case NodeWait:
		// A wait node only ever runs as the entry of a resumed run; its output
		// is the input RunGraph already checked.
		return func(ctx context.Context, in map[string]any) (*execOut, error) {
			nodeEvent(NodeEvent{Node: nd.ID, Kind: NodeStarted})
			var result Result = NewTextResult(resumeOut, "stop", 0, 0)
			out := &execOut{result: result}
			if nd.Wait != nil && nd.Wait.Expect == ExpectChoice {
				out.route = resumeOut
			}
			if nd.Wait != nil && nd.Wait.Record != "" {
				author := "participant"
				if req.Action != nil && req.Action.ParticipantID != "" {
					author = "participant:" + req.Action.ParticipantID
				}
				step, err := e.steps.run(ctx, session, StepRequest{
					external:  result,
					author:    author,
					AgentSlug: nd.Wait.Record,
					Owner:     req.Owner,
					Action:    req.Action,
					Inputs:    in,
					turnID:    runID,
					turnRole:  role,
					bus:       bus,
				})
				if !stepSucceeded(step, err) {
					return nil, err
				}
				out.step = step
				return out, err
			}
			return out, nil
		}

	default: // NodeDecide
		dec := nd.Decide
		return func(ctx context.Context, in map[string]any) (*execOut, error) {
			nodeEvent(NodeEvent{Node: nd.ID, Kind: NodeStarted})
			key := dec.Over
			if key == "" {
				key = "action"
			}
			var subject string
			switch v := in[key].(type) {
			case string:
				subject = v
			case nil:
			default:
				subject = fmt.Sprint(v)
			}
			ev, err := e.Evaluate(ctx, subject, map[string]EvalQuestion{nd.ID: dec.Question})
			if err != nil {
				return nil, fmt.Errorf("noewright: decide %q: %w", nd.ID, err)
			}
			ans, ok := ev.Answers[nd.ID]
			if !ok {
				return nil, fmt.Errorf("noewright: decide %q: the evaluator returned no answer", nd.ID)
			}
			label, p := decisionLabel(ans)
			gs.mu.Lock()
			gs.evals[nd.ID] = map[string]any{
				"label":         label,
				"p":             p,
				"certainty":     ans.Certainty(),
				"noul":          ans.Noul,
				"score":         ans.Score,
				"probabilities": ans.Probabilities,
			}
			gs.mu.Unlock()
			return &execOut{result: NewTextResult(label, "stop", 0, 0), route: label}, nil
		}
	}
}

// decisionLabel turns an answer into the route value a decide node emits, and
// the probability of that value.
func decisionLabel(a EvalAnswer) (string, float64) {
	switch a.Type {
	case "noul":
		if a.Noul >= 0.5 {
			return "yes", a.Noul
		}
		return "no", 1 - a.Noul
	case "score":
		l := strconv.Itoa(a.Level())
		return l, a.P(l)
	default:
		return a.Choice, a.P(a.Choice)
	}
}

// graphNodeChoose builds the closure that applies node i's Save mappings and
// function vars, and picks which of its edges fire. It runs on the
// coordinating goroutine, before any successor starts.
func (e *Engine) graphNodeChoose(session *Session, c *compiledGraph, gs *graphRunState, i int, inputs map[string]any) func(*execOut, bool) []bool {
	nd := c.g.Nodes[i]
	return func(out *execOut, ok bool) []bool {
		env := func() map[string]any {
			gs.mu.Lock()
			data, evals, failed := maps.Clone(gs.data), maps.Clone(gs.evals), maps.Clone(gs.failed)
			gs.mu.Unlock()
			e.mu.Lock()
			vars := maps.Clone(session.State.Vars)
			e.mu.Unlock()
			if vars == nil {
				vars = map[string]any{}
			}
			gs.mu.Lock()
			visits := map[string]any{}
			for k, v := range gs.visits {
				visits[k] = v
			}
			gs.mu.Unlock()
			return map[string]any{"inputs": inputs, "data": data, "vars": vars, "eval": evals,
				"run": map[string]any{"errors": failed}, "visits": visits}
		}

		// apply writes vars through their reducers.
		apply := func(writes map[string]any) {
			if len(writes) == 0 {
				return
			}
			e.mu.Lock()
			if session.State.Vars == nil {
				session.State.Vars = map[string]any{}
			}
			for v, val := range writes {
				session.State.Vars[v] = reduceVar(c.g.Vars[v].Reducer, session.State.Vars[v], val)
			}
			e.mu.Unlock()
			gs.mu.Lock()
			gs.changed = true
			gs.mu.Unlock()
		}
		if ok {
			// A function's own vars first, then the node's Save mappings, which
			// see the vars as the function left them.
			gs.mu.Lock()
			fv := gs.funcVars[i]
			gs.mu.Unlock()
			apply(fv)
			if len(c.saves[i]) > 0 {
				ev := env()
				writes := map[string]any{}
				for v, p := range c.saves[i] {
					val, err := expr.Run(p, ev)
					if err != nil {
						gs.mu.Lock()
						gs.edgeErrs[nd.ID+".save."+v] = fmt.Errorf("noewright: node %q save %q: %w", nd.ID, v, err)
						gs.mu.Unlock()
						continue
					}
					writes[v] = val
				}
				apply(writes)
			}
		}

		var fire []bool
		var hops []GraphHop
		take := func(k int, reason string) {
			for _, to := range c.edges[k].to {
				hops = append(hops, GraphHop{From: nd.ID, To: c.g.Nodes[to].ID, Reason: reason})
			}
		}
		fired := map[int]string{} // edge index → reason
		normal := ok || nd.Continue
		if !ok {
			for _, k := range c.out[i] {
				if c.edges[k].OnError {
					fired[k] = "error"
				}
			}
		}
		if normal {
			var ev map[string]any
			uncertain := false
			if out != nil && nd.Kind == NodeDecide {
				gs.mu.Lock()
				summary, _ := gs.evals[nd.ID].(map[string]any)
				gs.mu.Unlock()
				cert, _ := summary["certainty"].(float64)
				for _, k := range c.out[i] {
					if u := c.edges[k].Uncertain; u > 0 && cert < u {
						fired[k], uncertain = "uncertain", true
					}
				}
			}
			for _, k := range c.out[i] {
				ed := c.edges[k]
				switch {
				case ed.OnError, ed.Default, ed.Uncertain > 0:
				case ed.Route != "":
					if !uncertain && out != nil && out.route == ed.Route {
						fired[k] = "route"
					}
				case ed.when != nil:
					if ev == nil {
						ev = env()
					}
					v, err := expr.Run(ed.when, ev)
					if err != nil {
						gs.mu.Lock()
						gs.edgeErrs[fmt.Sprintf("%s.edge%d", nd.ID, k)] = fmt.Errorf("noewright: edge from %q: when: %w", nd.ID, err)
						gs.mu.Unlock()
						continue
					}
					if b, _ := v.(bool); b {
						fired[k] = "when"
					}
				default:
					fired[k] = ""
				}
			}
			if len(fired) == 0 {
				for _, k := range c.out[i] {
					if c.edges[k].Default {
						fired[k] = "default"
					}
				}
			}
		}

		for _, k := range c.out[i] {
			reason, f := fired[k]
			for _, to := range c.edges[k].to {
				if c.wait[to] {
					// Not an executor edge: firing it pauses the run there.
					if f {
						gs.mu.Lock()
						gs.pauses = append(gs.pauses, to)
						gs.mu.Unlock()
					}
					continue
				}
				fire = append(fire, f)
			}
			if f {
				take(k, reason)
			}
		}
		if len(hops) > 0 {
			gs.mu.Lock()
			gs.path = append(gs.path, hops...)
			gs.mu.Unlock()
			for _, h := range hops {
				e.emit(context.Background(), SessionEvent{Type: EventGraphHop, SessionID: session.ID, StepIndex: -1, Node: h.From, Hop: &h})
			}
		}
		return fire
	}
}

// reduceVar combines val into the current value cur by reducer r.
func reduceVar(r Reducer, cur, val any) any {
	switch r {
	case ReduceAppend:
		var list []any
		if rv := reflect.ValueOf(cur); cur != nil && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
			for k := range rv.Len() {
				list = append(list, rv.Index(k).Interface())
			}
		}
		if rv := reflect.ValueOf(val); val != nil && rv.Kind() == reflect.Slice {
			for k := range rv.Len() {
				list = append(list, rv.Index(k).Interface())
			}
			return list
		}
		return append(list, val)
	case ReduceMerge:
		m := map[string]any{}
		if cm, ok := cur.(map[string]any); ok {
			maps.Copy(m, cm)
		}
		if vm, ok := val.(map[string]any); ok {
			maps.Copy(m, vm)
			return m
		}
		return val
	case ReduceSum:
		return toFloat(cur) + toFloat(val) // ints and floats; anything else counts as 0
	default:
		return val
	}
}

// runCost totals the cost rows recorded for steps.
func (e *Engine) runCost(ctx context.Context, steps []*Step) (UsageSummary, error) {
	sum := UsageSummary{ByModel: map[string]float64{}, PricingConfigured: true}
	if len(steps) == 0 {
		return sum, nil
	}
	args := make([]any, len(steps))
	marks := make([]string, len(steps))
	for i, st := range steps {
		args[i] = st.ID
		marks[i] = fmt.Sprintf("$%d", i+1)
	}
	rows, err := e.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT model, input_tokens, output_tokens, usd_cost, estimated FROM %scost_records WHERE step_id IN (%s)`,
		e.prefix, strings.Join(marks, ",")), args...)
	if err != nil {
		return sum, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			model     string
			in, out   int
			usd       float64
			estimated bool
		)
		if err := rows.Scan(&model, &in, &out, &usd, &estimated); err != nil {
			return sum, err
		}
		sum.TotalUSD += usd
		sum.TotalTokens += in + out
		sum.StepCount++
		sum.ByModel[model] += usd
		if estimated {
			sum.PricingConfigured = false
		}
	}
	return sum, rows.Err()
}
