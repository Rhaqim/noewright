package engine

// graph_exec.go — the one executor behind RunTurn (and, later, RunGraph).
//
// A graph here is a set of nodes and the edges between them. The executor owns
// scheduling only: when a node may start, what inputs it sees, which edges fire
// when it settles, and what is recorded. What a node DOES is a closure supplied
// by the caller (an agent step today; a Go function or an Evaluator decision
// later), so RunTurn and RunGraph share one scheduler without sharing policy.
//
// Scheduling rules:
//
//   - The entry node is active from the start. Any other node is active once at
//     least one incoming control edge has fired. Data edges only order: a node
//     waits for the producers its data edges come from, but they never start it.
//   - A node starts when every incoming edge is resolved: fired, or dead because
//     its source settled without firing it or was itself never activated
//     ("dead-path elimination"). A node all of whose incoming edges are dead is
//     dead too, and its own outgoing edges die with it.
//   - Nodes that become ready together are launched together, each with a clone
//     of the shared inputs taken at launch. Outputs are written to the shared
//     inputs only by the coordinating goroutine, when a node settles, so a node
//     never observes a sibling launched alongside it.
//   - A lone ready node with nothing running runs on the caller's goroutine. That
//     keeps a single-node chain (a turn's lead) free of goroutine hops, and a
//     node that does not recover its panics propagates them to the caller
//     exactly as a direct call would.
//   - When a required node fails, nothing further is launched; nodes already
//     running are allowed to finish and the run reports the failure.

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// execOut is what a node produced. step is nil for a node that records no step
// (a function or a decision); result is the node's output either way.
type execOut struct {
	step   *Step
	result Result
	route  string // the route value edges can match ("" = none)
}

// execNode is one node of an executable graph.
type execNode struct {
	// outputKey, when non-empty, names the shared input the node's successful
	// output text is written to as it settles.
	outputKey string
	// inputs are merged over the shared inputs for this node only.
	inputs map[string]any
	// required makes a failure of this node fail the whole run.
	required bool
	// fireOnFail fires the node's outgoing edges even when it fails or is
	// skipped, so its consumers still run (and see its output missing). Without
	// it, a failed or skipped node ends its own branch.
	fireOnFail bool
	// recoverPanics turns a panic inside the node (run, gate or settled) into a
	// node failure built by onPanic. Without it a panic propagates.
	recoverPanics bool
	onPanic       func(r any) error

	// gate, when set, decides whether the node runs at all. A false answer
	// skips it (no step, no settled call); an error skips it and is recorded,
	// and settled is called with (nil, err).
	gate func(ctx context.Context, in map[string]any) (bool, error)
	// run does the node's work. A non-nil out alongside ErrSessionNotPersisted
	// counts as success: the step committed and only the session row is stale.
	run func(ctx context.Context, in map[string]any) (*execOut, error)
	// settled is called on the node's own goroutine after its outcome is
	// recorded and before any successor can start, with run's raw (out, err).
	settled func(out *execOut, err error)
	// choose, when set, decides which outgoing edges fire once the node has
	// run (fire[k] is for succ[i][k]; data edges ignore it). It is called on the
	// coordinating goroutine after the node's output is written to the shared
	// inputs, with ok reporting success. When nil, every edge fires on success,
	// and on failure only if fireOnFail. A gated-out node never calls it.
	choose func(out *execOut, ok bool) []bool
}

// execEdge is one outgoing edge. A data edge orders its target after its source
// but never activates it.
type execEdge struct {
	to   int
	data bool
}

// execGraph is a set of nodes, their outgoing edges, and the entry node.
type execGraph struct {
	nodes []execNode
	succ  [][]execEdge // succ[i] = node i's outgoing edges
	entry int
}

// execError is one recorded node error, kept in the order it was recorded.
type execError struct {
	node int
	err  error
}

// execResult is what a run produced.
type execResult struct {
	steps     []*Step // recorded steps, in completion order
	stepNodes []int   // stepNodes[k] is the node that produced steps[k]
	outOf     map[int]*execOut
	errs      []execError // in the order they were recorded
	skipped   []int       // gated-out nodes, in completion order
	dead      []int       // nodes never activated, in the order found dead
	ran       []int       // nodes that ran (succeeded or failed), in completion order
	abort     error       // set when a required node failed
	abortNode int
}

// stepSucceeded reports whether run's (step, err) counts as a success.
func stepSucceeded(step *Step, err error) bool {
	return step != nil && (err == nil || errors.Is(err, ErrSessionNotPersisted))
}

// outSucceeded reports whether a node's (out, err) counts as a success.
func outSucceeded(out *execOut, err error) bool {
	return out != nil && (err == nil || errors.Is(err, ErrSessionNotPersisted))
}

// stepOut wraps an agent step as a node output; nil for a failed step.
func stepOut(step *Step, err error) *execOut {
	if step == nil {
		return nil
	}
	return &execOut{step: step, result: step.Result}
}

// runExecGraph runs g against the shared inputs map, which it updates with each
// node's output as the node settles. inputs must not be touched by anyone else
// until the call returns.
func runExecGraph(ctx context.Context, g *execGraph, inputs map[string]any) *execResult {
	n := len(g.nodes)
	pred := make([]int, n) // number of incoming edges per node
	for _, ss := range g.succ {
		for _, e := range ss {
			pred[e.to]++
		}
	}
	resolved := make([]int, n)
	fired := make([]int, n)

	res := &execResult{outOf: map[int]*execOut{}}
	var mu sync.Mutex // guards res while nodes run concurrently

	// resolve marks node i's outgoing edges fired or dead (fire[k] for edge k;
	// nil = none fire) and returns the nodes that became ready. Dead nodes are
	// propagated through immediately.
	resolve := func(i int, fire []bool) []int {
		var ready []int
		type settledEdges struct {
			from int
			fire []bool
		}
		queue := []settledEdges{{i, fire}}
		for len(queue) > 0 {
			src := queue[0]
			queue = queue[1:]
			for k, e := range g.succ[src.from] {
				j := e.to
				if j == g.entry {
					// The entry runs from the start, whatever its edges do. An
					// edge into it (a loop back through a wait node) settles
					// without making it ready or dead a second time.
					continue
				}
				resolved[j]++
				if !e.data && src.fire != nil && src.fire[k] {
					fired[j]++
				}
				if resolved[j] < pred[j] {
					continue
				}
				if fired[j] > 0 {
					ready = append(ready, j)
				} else {
					res.dead = append(res.dead, j)
					queue = append(queue, settledEdges{j, nil}) // dead: its edges die too
				}
			}
		}
		return ready
	}
	// uniform fires every edge of node i, or none.
	uniform := func(i int, fire bool) []bool {
		if !fire {
			return nil
		}
		f := make([]bool, len(g.succ[i]))
		for k := range f {
			f[k] = true
		}
		return f
	}

	// execOne runs node i with its launch-time inputs and reports whether it
	// ran (false = gated out), and whether it succeeded.
	execOne := func(i int, in map[string]any) (ran, ok bool) {
		node := &g.nodes[i]
		succeeded, settledCalled := false, false
		if node.recoverPanics {
			defer func() {
				r := recover()
				if r == nil {
					return
				}
				perr := node.onPanic(r)
				mu.Lock()
				if !succeeded {
					res.errs = append(res.errs, execError{i, perr})
				}
				if !slices.Contains(res.ran, i) {
					res.ran = append(res.ran, i)
				}
				mu.Unlock()
				if node.settled != nil && !settledCalled {
					node.settled(nil, perr)
				}
				ran, ok = true, succeeded
			}()
		}

		if node.gate != nil {
			run, gerr := node.gate(ctx, in)
			if gerr != nil {
				mu.Lock()
				res.errs = append(res.errs, execError{i, gerr})
				res.skipped = append(res.skipped, i)
				mu.Unlock()
				if node.settled != nil {
					settledCalled = true
					node.settled(nil, gerr)
				}
				return false, false
			}
			if !run {
				mu.Lock()
				res.skipped = append(res.skipped, i)
				mu.Unlock()
				return false, false
			}
		}

		out, err := node.run(ctx, in)
		succeeded = outSucceeded(out, err)
		mu.Lock()
		res.ran = append(res.ran, i)
		if err != nil {
			res.errs = append(res.errs, execError{i, err})
		}
		if succeeded {
			res.outOf[i] = out
			if out.step != nil {
				res.steps = append(res.steps, out.step)
				res.stepNodes = append(res.stepNodes, i)
			}
		}
		mu.Unlock()
		if node.settled != nil {
			settledCalled = true
			node.settled(out, err)
		}
		return true, succeeded
	}

	type settledNode struct {
		node    int
		ran, ok bool
		err     error
	}
	done := make(chan settledNode, n)
	running := 0
	aborted := false
	ready := []int{g.entry}
	// A node other than the entry with no incoming edge can never be reached in
	// this run (a resumed graph starts mid-way, so the nodes before it have no
	// way in): it is dead from the start, and so are the edges leaving it.
	for i := range n {
		if i != g.entry && pred[i] == 0 {
			res.dead = append(res.dead, i)
			ready = append(ready, resolve(i, nil)...)
		}
	}

	// settle handles a node's outcome on the coordinating goroutine.
	settle := func(s settledNode) {
		node := &g.nodes[s.node]
		if !s.ok && node.required {
			aborted = true
			mu.Lock()
			res.abort, res.abortNode = s.err, s.node
			mu.Unlock()
			return
		}
		mu.Lock()
		out := res.outOf[s.node]
		mu.Unlock()
		if s.ok && node.outputKey != "" && out.result != nil {
			inputs[node.outputKey] = ResultText(out.result)
		}
		var fire []bool
		switch {
		case !s.ran: // gated out
			fire = uniform(s.node, node.fireOnFail)
		case node.choose != nil:
			fire = node.choose(out, s.ok)
		default:
			fire = uniform(s.node, s.ok || node.fireOnFail)
		}
		ready = append(ready, resolve(s.node, fire)...)
	}
	// lastErr returns the most recent error recorded for node i, or a generic
	// one when the node failed without recording any (a gated-out required node).
	lastErr := func(i int) error {
		mu.Lock()
		defer mu.Unlock()
		for k := len(res.errs) - 1; k >= 0; k-- {
			if res.errs[k].node == i {
				return res.errs[k].err
			}
		}
		return errors.New("noewright: required node did not run")
	}

	for {
		if !aborted {
			batch := ready
			ready = nil
			if len(batch) == 1 && running == 0 {
				i := batch[0]
				ran, ok := execOne(i, agentInputs(inputs, g.nodes[i].inputs))
				s := settledNode{node: i, ran: ran, ok: ok}
				if !ok {
					s.err = lastErr(i)
				}
				settle(s)
				continue
			}
			for _, i := range batch {
				in := agentInputs(inputs, g.nodes[i].inputs) // cloned at launch
				running++
				go func(i int, in map[string]any) {
					ran, ok := execOne(i, in)
					s := settledNode{node: i, ran: ran, ok: ok}
					if !ok {
						s.err = lastErr(i)
					}
					done <- s
				}(i, in)
			}
		}
		if running == 0 {
			break
		}
		s := <-done
		running--
		settle(s)
	}
	return res
}
