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
//     least one incoming edge has fired.
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
	"sync"
)

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
	// run does the node's work. A non-nil step alongside ErrSessionNotPersisted
	// counts as success: the step committed and only the session row is stale.
	run func(ctx context.Context, in map[string]any) (*Step, error)
	// settled is called on the node's own goroutine after its outcome is
	// recorded and before any successor can start, with run's raw (step, err).
	settled func(step *Step, err error)
}

// execGraph is a set of nodes, their outgoing edges, and the entry node.
type execGraph struct {
	nodes []execNode
	succ  [][]int // succ[i] = nodes that node i's edges lead to
	entry int
}

// execError is one recorded node error, kept in the order it was recorded.
type execError struct {
	node int
	err  error
}

// execResult is what a run produced.
type execResult struct {
	steps     []*Step // successful steps, in completion order
	stepNodes []int   // stepNodes[k] is the node that produced steps[k]
	stepOf    map[int]*Step
	errs      []execError // in the order they were recorded
	skipped   []int       // gated-out nodes, in completion order
	abort     error       // set when a required node failed
}

// stepSucceeded reports whether run's (step, err) counts as a success.
func stepSucceeded(step *Step, err error) bool {
	return step != nil && (err == nil || errors.Is(err, ErrSessionNotPersisted))
}

// runExecGraph runs g against the shared inputs map, which it updates with each
// node's output as the node settles. inputs must not be touched by anyone else
// until the call returns.
func runExecGraph(ctx context.Context, g *execGraph, inputs map[string]any) *execResult {
	n := len(g.nodes)
	pred := make([]int, n) // number of incoming edges per node
	for _, ss := range g.succ {
		for _, j := range ss {
			pred[j]++
		}
	}
	resolved := make([]int, n)
	fired := make([]int, n)

	res := &execResult{stepOf: map[int]*Step{}}
	var mu sync.Mutex // guards res while nodes run concurrently

	// resolve marks node i's outgoing edges fired or dead and returns the nodes
	// that became ready. Dead nodes are propagated through immediately.
	resolve := func(i int, fire bool) []int {
		var ready []int
		type edge struct {
			from int
			fire bool
		}
		queue := []edge{{i, fire}}
		for len(queue) > 0 {
			e := queue[0]
			queue = queue[1:]
			for _, j := range g.succ[e.from] {
				resolved[j]++
				if e.fire {
					fired[j]++
				}
				if resolved[j] < pred[j] {
					continue
				}
				if fired[j] > 0 {
					ready = append(ready, j)
				} else {
					queue = append(queue, edge{j, false}) // dead: its edges die too
				}
			}
		}
		return ready
	}

	// execOne runs node i with its launch-time inputs and reports whether its
	// outgoing edges fire and whether it succeeded.
	execOne := func(i int, in map[string]any) (fire, ok bool) {
		node := &g.nodes[i]
		succeeded, settledCalled := false, false
		if node.recoverPanics {
			defer func() {
				r := recover()
				if r == nil {
					return
				}
				perr := node.onPanic(r)
				if !succeeded {
					mu.Lock()
					res.errs = append(res.errs, execError{i, perr})
					mu.Unlock()
				}
				if node.settled != nil && !settledCalled {
					node.settled(nil, perr)
				}
				fire, ok = succeeded || node.fireOnFail, succeeded
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
				return node.fireOnFail, false
			}
			if !run {
				mu.Lock()
				res.skipped = append(res.skipped, i)
				mu.Unlock()
				return node.fireOnFail, false
			}
		}

		step, err := node.run(ctx, in)
		succeeded = stepSucceeded(step, err)
		mu.Lock()
		if err != nil {
			res.errs = append(res.errs, execError{i, err})
		}
		if succeeded {
			res.stepOf[i] = step
			res.steps = append(res.steps, step)
			res.stepNodes = append(res.stepNodes, i)
		}
		mu.Unlock()
		if node.settled != nil {
			settledCalled = true
			node.settled(step, err)
		}
		return succeeded || node.fireOnFail, succeeded
	}

	type settledNode struct {
		node     int
		fire, ok bool
		err      error
	}
	done := make(chan settledNode, n)
	running := 0
	aborted := false
	ready := []int{g.entry}

	// settle handles a node's outcome on the coordinating goroutine.
	settle := func(s settledNode) {
		node := &g.nodes[s.node]
		if !s.ok && node.required {
			aborted = true
			mu.Lock()
			res.abort = s.err
			mu.Unlock()
			return
		}
		if s.ok && node.outputKey != "" {
			mu.Lock()
			step := res.stepOf[s.node]
			mu.Unlock()
			inputs[node.outputKey] = ResultText(step.Result)
		}
		ready = append(ready, resolve(s.node, s.fire)...)
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
				fire, ok := execOne(i, agentInputs(inputs, g.nodes[i].inputs))
				s := settledNode{node: i, fire: fire, ok: ok}
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
					fire, ok := execOne(i, in)
					s := settledNode{node: i, fire: fire, ok: ok}
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
