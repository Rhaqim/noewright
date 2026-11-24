package engine

// api_graph.go — graphs: agents, Go functions and Evaluator decisions joined
// by edges that can branch, fan out and join again.
//
// A Flow is a graph with a fixed shape (a lead, then follower layers) and
// RunTurn runs it on the same executor. A Graph lifts the shape restriction:
// any node can feed one or more nodes, edges can carry conditions, and branches
// that started in a run are joined again before a node they all lead to runs.

import (
	"context"

	"github.com/google/uuid"
)

// GraphNodeKind names what a graph node does.
type GraphNodeKind string

const (
	// NodeAgent runs an agent and records a step.
	NodeAgent GraphNodeKind = "agent"
	// NodeFunc calls a Go function registered with Engine.RegisterFunc. It
	// records a step only when GraphNode.Record names an agent to attribute it to.
	NodeFunc GraphNodeKind = "func"
	// NodeDecide asks the Evaluator one question and emits the answer as the
	// node's route value. It records no step.
	NodeDecide GraphNodeKind = "decide"
)

// Graph is a set of nodes joined by edges, run with Engine.RunGraph.
//
// Edges come from two places. Control edges are written in Edges; they start
// nodes and carry routing. Data edges are inferred: when an agent node's user
// template declares a variable (Prompt.Variables) that another node produces
// under its output key, the consumer runs after the producer. Data edges order
// nodes but never start one.
//
// A node runs once at least one control edge into it has fired and every edge
// into it is settled — fired, or dead because its source finished without
// firing it or never ran. So a node several branches lead to waits for the
// branches that actually started in this run, and no longer.
type Graph struct {
	Slug  string
	Entry string // id of the entry node
	// Inputs names the inputs a caller supplies at run time (GraphRequest.Inputs),
	// as opposed to outputs produced inside the graph. "action" is always
	// available. Validate treats these as satisfied.
	Inputs []string
	// Vars declares how a session var written by more than one node is combined.
	// A var with no entry is replaced by the last write. Validate refuses a
	// graph where two nodes that can run in parallel Save the same undeclared
	// var.
	Vars  map[string]GraphVar
	Nodes []GraphNode
	Edges []GraphEdge
}

// Reducer combines a new value into a session var.
type Reducer string

const (
	// ReduceReplace overwrites the var (the default).
	ReduceReplace Reducer = "replace"
	// ReduceAppend appends to a list, creating it if absent.
	ReduceAppend Reducer = "append"
	// ReduceMerge merges a map's keys into the var's map.
	ReduceMerge Reducer = "merge"
	// ReduceSum adds a number to the var.
	ReduceSum Reducer = "sum"
)

// GraphVar declares a session var's reducer.
type GraphVar struct {
	Reducer Reducer
}

// GraphNode is one node of a Graph. Set the fields for its Kind.
type GraphNode struct {
	ID   string
	Kind GraphNodeKind

	// Agent configures a NodeAgent: the agent, its output key, whether it
	// streams, its params, overrides, retry policy and own inputs, exactly as
	// for a turn's agents. Agent.When is not used in graphs; route with edge
	// conditions instead.
	Agent *FlowAgent

	// Func names the registered function a NodeFunc calls.
	Func string
	// Record, on a NodeFunc, saves the function's output as a step attributed
	// to this agent slug (Owner-scoped, latest version), the way RecordStep
	// does. Empty records no step.
	Record string

	// Decide configures a NodeDecide.
	Decide *GraphDecision

	// OutputKey names the input a NodeFunc or NodeDecide output is exposed under
	// to later nodes. A NodeAgent uses Agent.OutputKey.
	OutputKey string

	// Save assigns session vars from expressions once the node succeeds:
	// var name → expression. The value is combined through the var's reducer.
	Save map[string]string

	// Required makes a failure of this node fail the whole run. Otherwise a
	// failure ends the node's own branch, unless an OnError edge leaves it or
	// Continue is set.
	Required bool
	// Continue evaluates the node's ordinary edges even when it fails, so its
	// consumers still run and see its output missing.
	Continue bool
}

// GraphDecision is the question a NodeDecide asks.
type GraphDecision struct {
	// Question is an Evaluator question, built with EvalChoice, EvalNoul or
	// EvalScore. It is asked under the node's ID.
	Question EvalQuestion
	// Over names the input whose text is evaluated, usually an earlier node's
	// output key. Empty evaluates the action text.
	Over string
}

// GraphEdge joins one node to one or more nodes. At most one of Route, When,
// Uncertain, Default and OnError may be set; with none the edge always fires.
//
// When a node settles, every matching edge fires, so several edges can fire at
// once. Default fires when no other edge from the node did. When an Uncertain
// edge fires, the node's Route edges do not.
type GraphEdge struct {
	From string
	To   []string
	// Route fires when the node's route value equals it. A NodeDecide emits the
	// chosen option (Choice), "yes" or "no" (Noul), or the level index (Score);
	// a NodeFunc emits FuncResult.Route.
	Route string
	// When fires when the expression is true. See the expression environment
	// on GraphRun.
	When string
	// Uncertain, on an edge from a NodeDecide, fires when the answer's
	// certainty is below this value (0..1).
	Uncertain float64
	// Default fires when no other edge from the node fired.
	Default bool
	// OnError fires when the node failed.
	OnError bool
}

// GraphFunc is a Go function a NodeFunc calls. It must be safe to call
// concurrently: parallel branches may call it at the same time.
type GraphFunc func(ctx context.Context, call FuncCall) (FuncResult, error)

// FuncCall is what a GraphFunc receives.
type FuncCall struct {
	Node   string         // the node being run
	Inputs map[string]any // this node's own copy of the run inputs
	Vars   map[string]any // a snapshot of the session vars
	// Session is the session the run belongs to. Treat it as read-only; change
	// vars through FuncResult.Vars so reducers apply.
	Session *Session
}

// FuncResult is what a GraphFunc returns.
type FuncResult struct {
	// Text is the function's output, exposed under the node's output key.
	Text string
	// Data, when set, makes the output structured: later nodes read its fields
	// as data.<OutputKey>.<field> in expressions.
	Data map[string]any
	// Route is the route value the node's edges can match.
	Route string
	// Vars are written to the session through each var's reducer.
	Vars map[string]any
}

// GraphRequest is the input to RunGraph.
type GraphRequest struct {
	Graph Graph
	// Owner scopes every agent lookup in the run, as TurnRequest.Owner does.
	Owner string
	// Action is the user input driving the run. It is recorded on the entry
	// node's step when the entry is an agent, and its text is available to
	// every node as the "action" input.
	Action *Action
	// Inputs seed the run's inputs, available to every node.
	Inputs map[string]any
	// Params and Overrides apply to every agent node, under each node's own.
	Params    map[string]any
	Overrides map[string]any
	// OnChunk receives the chunks of every agent node that streams.
	OnChunk func(node string, c Chunk)
	// OnNode receives each node's progress. It is called from the node's own
	// goroutine and must not block.
	OnNode func(ev NodeEvent)
}

// NodeEventKind names a NodeEvent.
type NodeEventKind string

const (
	NodeStarted NodeEventKind = "started"
	NodeDone    NodeEventKind = "done"
	NodeFailed  NodeEventKind = "failed"
	// NodeSkipped: no edge into the node fired, so it did not run.
	NodeSkipped NodeEventKind = "skipped"
)

// NodeEvent is one node's progress in a run.
type NodeEvent struct {
	Node  string
	Kind  NodeEventKind
	Step  *Step  // the recorded step, when the node recorded one
	Text  string // the node's output text, on NodeDone
	Route string // the node's route value, on NodeDone
	Err   error  // on NodeFailed
}

// GraphHop is one edge a run took.
type GraphHop struct {
	From, To string
	// Reason says why the edge fired: "" (unconditional), "route", "when",
	// "uncertain", "default" or "error".
	Reason string
}

// GraphRun is the result of RunGraph.
//
// Expressions in edges and Save mappings see:
//
//	inputs   run inputs and every output produced so far, by key
//	data     structured outputs by key (data.Ticket.priority)
//	vars     the session vars
//	eval     decision results by node id: label, p, certainty, noul, score,
//	         probabilities
//	run      errors: node id → error message, for nodes failed so far
type GraphRun struct {
	ID       uuid.UUID
	Steps    []*Step          // recorded steps, in completion order
	Nodes    map[string]*Step // by node id, nodes that recorded a step
	Outputs  map[string]any   // every input and output key at the end of the run
	Errors   map[string]error // by node id
	Skipped  []string         // nodes no edge reached
	Path     []GraphHop       // every edge taken, in the order taken
	Messages []Message        // everything published on the run's bus
	// Cost totals the cost rows of the run's steps. Recorded steps (functions
	// with Record) have none. PricingConfigured is false when any row was priced
	// from the placeholder rate: show the USD as unknown then.
	Cost UsageSummary
}
