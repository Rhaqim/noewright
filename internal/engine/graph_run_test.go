package engine

// graph_run_test.go — RunGraph: branching, joins, functions, decisions,
// saves and reducers, failure policies, and the checks made before a run.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/rhaqim/noewright/evaluator"
	"github.com/rhaqim/noewright/evaluator/stub"
)

func graphEngine(t *testing.T, name string) *Engine {
	t.Helper()
	e, _ := reproEngine(t, name, map[string]Generator{"echo": echoPromptGen{}, "fail": downGen{}, "panic": panicGen{}}, PollerConfig{})
	return e
}

func newSession(t *testing.T, e *Engine, vars map[string]any) *Session {
	t.Helper()
	s := &Session{PlatformID: "p", State: State{Vars: vars}}
	if err := e.Sessions().Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

func agentNode(id, slug, out string) GraphNode {
	return GraphNode{ID: id, Kind: NodeAgent, Agent: &FlowAgent{AgentSlug: slug, OutputKey: out}}
}

func to(ids ...string) []string { return ids }

func sortedSkipped(r *GraphRun) []string {
	s := slices.Clone(r.Skipped)
	sort.Strings(s)
	return s
}

// The tabletop round from the demos: the rules check and the dice are skipped
// on the opening round, and the game master waits for the dice when they run.
func TestRunGraph_TabletopRound(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "graph_tabletop")
	mkGenAgent(t, e, "tt-rules", "echo", "RULES[{{.Inputs.action}}]", nil)
	mkGenAgent(t, e, "tt-gm", "echo", "GM[{{.Inputs.Rolls}}]", nil)
	mkGenAgent(t, e, "tt-referee", "echo", "REF[{{.Inputs.Narration}}]", []string{"Narration"})
	mkGenAgent(t, e, "tt-chronicler", "echo", "CHRON[{{.Inputs.Narration}}]", []string{"Narration"})
	mkGenAgent(t, e, "tt-suggest", "echo", "SUGG[{{.Inputs.Narration}}]", []string{"Narration"})

	var rolled sync.Mutex
	rolls := 0
	e.RegisterFunc("start", func(context.Context, FuncCall) (FuncResult, error) { return FuncResult{}, nil })
	e.RegisterFunc("roll-dice", func(_ context.Context, call FuncCall) (FuncResult, error) {
		rolled.Lock()
		rolls++
		rolled.Unlock()
		return FuncResult{Text: "d20=17 for " + fmt.Sprint(call.Inputs["Rules"])}, nil
	})

	g := Graph{
		Slug:   "tabletop-round",
		Entry:  "start",
		Inputs: []string{"Rolls"},
		Nodes: []GraphNode{
			{ID: "start", Kind: NodeFunc, Func: "start"},
			agentNode("rules", "tt-rules", "Rules"),
			{ID: "roll", Kind: NodeFunc, Func: "roll-dice", OutputKey: "Rolls"},
			{ID: "gm", Kind: NodeAgent, Agent: &FlowAgent{AgentSlug: "tt-gm", OutputKey: "Narration", Stream: true}},
			agentNode("referee", "tt-referee", "Ref"),
			agentNode("chronicler", "tt-chronicler", "Chron"),
			agentNode("suggest", "tt-suggest", "Sugg"),
		},
		Edges: []GraphEdge{
			{From: "start", To: to("rules"), When: "vars.round > 0"},
			{From: "start", To: to("gm"), Default: true},
			{From: "rules", To: to("roll")},
			{From: "roll", To: to("gm")},
			{From: "gm", To: to("referee", "chronicler", "suggest")},
		},
	}

	t.Run("opening round skips rules and dice", func(t *testing.T) {
		s := newSession(t, e, map[string]any{"round": 0})
		run, err := e.RunGraph(ctx, s, GraphRequest{Graph: g, Inputs: map[string]any{"Rolls": "no checks"},
			Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "we enter"}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := sortedSkipped(run); !slices.Equal(got, []string{"roll", "rules"}) {
			t.Fatalf("skipped = %v, want [roll rules]", got)
		}
		if got := ResultText(run.Nodes["gm"].Result); got != "GM[no checks]" {
			t.Fatalf("gm = %q", got)
		}
		if got := ResultText(run.Nodes["referee"].Result); got != "REF[GM[no checks]]" {
			t.Fatalf("referee = %q", got)
		}
		if len(run.Steps) != 4 {
			t.Fatalf("steps = %d, want 4 (gm + 3 followers; functions record none)", len(run.Steps))
		}
	})

	t.Run("later round runs rules, then dice, then gm", func(t *testing.T) {
		s := newSession(t, e, map[string]any{"round": 3})
		run, err := e.RunGraph(ctx, s, GraphRequest{Graph: g, Inputs: map[string]any{"Rolls": "unused"},
			Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "I pick the lock"}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(run.Skipped) != 0 {
			t.Fatalf("skipped = %v, want none", run.Skipped)
		}
		// gm read the dice output, so it ran after roll even though the start→gm
		// default edge did not fire.
		want := "GM[d20=17 for RULES[I pick the lock]]"
		if got := ResultText(run.Nodes["gm"].Result); got != want {
			t.Fatalf("gm = %q, want %q", got, want)
		}
		if run.Nodes["gm"].Diagnostics["turn_role"] != "node:gm" || run.Nodes["gm"].Diagnostics["turn_id"] != run.ID.String() {
			t.Fatalf("gm diagnostics = %v", run.Nodes["gm"].Diagnostics)
		}
		var reasons []string
		for _, h := range run.Path {
			if h.From == "start" {
				reasons = append(reasons, h.To+":"+h.Reason)
			}
		}
		if !slices.Equal(reasons, []string{"rules:when"}) {
			t.Fatalf("hops from start = %v", reasons)
		}
	})
}

// A decision routes by the Evaluator's answer, fans out, and the branches join.
// A low-certainty answer takes the uncertain edge and suppresses route edges.
func TestRunGraph_DecideRoutesAndJoins(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "graph_decide")
	mkGenAgent(t, e, "triage", "echo", "TICKET:{{.Inputs.action}}", nil)
	mkGenAgent(t, e, "refund-policy", "echo", "POLICY", nil)
	mkGenAgent(t, e, "repro", "echo", "REPRO", nil)
	mkGenAgent(t, e, "kb-search", "echo", "ARTICLES", nil)
	mkGenAgent(t, e, "clarify", "echo", "WHICH ONE?", nil)
	mkGenAgent(t, e, "writer", "echo", "ANSWER", nil)

	g := Graph{
		Slug:  "support",
		Entry: "intake",
		Nodes: []GraphNode{
			agentNode("intake", "triage", "Ticket"),
			{ID: "intent", Kind: NodeDecide, OutputKey: "Intent", Decide: &GraphDecision{
				Question: EvalChoice("What does the customer want?", map[string]any{"refund": nil, "bug": nil, "question": nil}),
				Over:     "Ticket",
			}},
			agentNode("refund", "refund-policy", "Policy"),
			agentNode("repro", "repro", "Repro"),
			agentNode("search", "kb-search", "Articles"),
			agentNode("clarify", "clarify", "Clarify"),
			agentNode("answer", "writer", "Answer"),
		},
		Edges: []GraphEdge{
			{From: "intake", To: to("intent")},
			{From: "intent", Route: "refund", To: to("refund")},
			{From: "intent", Route: "bug", To: to("repro", "search")},
			{From: "intent", Route: "question", To: to("search")},
			{From: "intent", Uncertain: 0.6, To: to("clarify")},
			{From: "refund", To: to("answer")},
			{From: "repro", To: to("answer")},
			{From: "search", To: to("answer")},
		},
	}

	t.Run("bug fans out to repro and search, then joins", func(t *testing.T) {
		ev := stub.New().WithChoice("intent", "bug")
		e.SetEvaluator(ev)
		s := newSession(t, e, nil)
		var mu sync.Mutex
		var order []string
		run, err := e.RunGraph(ctx, s, GraphRequest{Graph: g,
			Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "the app crashes"}},
			OnNode: func(ev NodeEvent) {
				if ev.Kind == NodeDone {
					mu.Lock()
					order = append(order, ev.Node)
					mu.Unlock()
				}
			}})
		if err != nil {
			t.Fatal(err)
		}
		if got := sortedSkipped(run); !slices.Equal(got, []string{"clarify", "refund"}) {
			t.Fatalf("skipped = %v", got)
		}
		if order[len(order)-1] != "answer" || !slices.Contains(order, "repro") || !slices.Contains(order, "search") {
			t.Fatalf("done order = %v, want repro and search before answer", order)
		}
		if run.Outputs["Intent"] != "bug" {
			t.Fatalf("Intent output = %v", run.Outputs["Intent"])
		}
		// The decision was asked about the ticket text, under the node id.
		calls := ev.Calls()
		if len(calls) != 1 || calls[0].State != "TICKET:the app crashes" {
			t.Fatalf("evaluator calls = %+v", calls)
		}
		if _, ok := calls[0].Questions["intent"]; !ok {
			t.Fatalf("question not asked under the node id: %+v", calls[0].Questions)
		}
	})

	t.Run("uncertain answer asks instead of routing", func(t *testing.T) {
		e.SetEvaluator(stub.New().WithAnswer("intent", evaluator.Answer{
			Type: evaluator.TypeChoice, Choice: "refund", Confidence: 0.3,
			Probabilities: map[string]float64{"refund": 0.4, "bug": 0.35, "question": 0.25},
		}))
		s := newSession(t, e, nil)
		run, err := e.RunGraph(ctx, s, GraphRequest{Graph: g,
			Action: &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "hmm"}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, ran := run.Nodes["clarify"]; !ran {
			t.Fatal("clarify did not run")
		}
		if _, ran := run.Nodes["refund"]; ran {
			t.Fatal("refund ran although the answer was uncertain")
		}
		// answer is reached by no fired edge: refund, repro and search were all
		// skipped, so it is skipped too.
		if got := sortedSkipped(run); !slices.Equal(got, []string{"answer", "refund", "repro", "search"}) {
			t.Fatalf("skipped = %v", got)
		}
	})
}

// A run-wide generator override applies to agent nodes without their own.
func TestRunGraph_GeneratorOverride(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "graph_genoverride")
	e.RegisterGenerator("other", constGen("OTHER"))
	mkGenAgent(t, e, "a", "echo", "A", nil)
	mkGenAgent(t, e, "b", "echo", "B", nil)
	g := Graph{Slug: "o", Entry: "a",
		Nodes: []GraphNode{agentNode("a", "a", "A"), {ID: "b", Kind: NodeAgent, Agent: &FlowAgent{AgentSlug: "b", GeneratorOverride: "echo"}}},
		Edges: []GraphEdge{{From: "a", To: to("b")}}}
	run, err := e.RunGraph(ctx, newSession(t, e, nil), GraphRequest{Graph: g, GeneratorOverride: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if ResultText(run.Nodes["a"].Result) != "OTHER" || ResultText(run.Nodes["b"].Result) != "B" {
		t.Fatalf("a = %q (want the run's generator), b = %q (want its own)", ResultText(run.Nodes["a"].Result), ResultText(run.Nodes["b"].Result))
	}
}

type constGen string

func (constGen) Modality() Modality { return ModalityText }
func (c constGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return NewTextResult(string(c), "stop", 1, 1), nil
}

// Data edges are inferred from templates: a node reading another's output runs
// after it even when both are reached from the same parent.
func TestRunGraph_InferredDataEdge(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "graph_data")
	mkGenAgent(t, e, "lead", "echo", "L", nil)
	mkGenAgent(t, e, "a", "echo", "A", nil)
	mkGenAgent(t, e, "b", "echo", "B[{{.Inputs.A}}]", []string{"A"})
	g := Graph{Slug: "d", Entry: "lead",
		Nodes: []GraphNode{agentNode("lead", "lead", "Lead"), agentNode("b", "b", "B"), agentNode("a", "a", "A")},
		Edges: []GraphEdge{{From: "lead", To: to("b", "a")}}}
	for range 5 {
		run, err := e.RunGraph(ctx, newSession(t, e, nil), GraphRequest{Graph: g})
		if err != nil {
			t.Fatal(err)
		}
		if got := ResultText(run.Nodes["b"].Result); got != "B[A]" {
			t.Fatalf("b = %q: it ran before a", got)
		}
	}
}

// Save mappings and function vars go through reducers, parallel branches
// included, and are persisted with the session.
func TestRunGraph_SavesAndReducers(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "graph_saves")
	mkGenAgent(t, e, "lead", "echo", "L", nil)
	mkGenAgent(t, e, "x", "echo", "X", nil)
	mkGenAgent(t, e, "y", "echo", "Y", nil)
	e.RegisterFunc("score", func(_ context.Context, c FuncCall) (FuncResult, error) {
		if string(c.Snapshot) != `{"sheet":1}` {
			return FuncResult{}, fmt.Errorf("snapshot = %q", c.Snapshot)
		}
		c.Snapshot[0] = 'X' // a copy: changing it must not touch the session
		return FuncResult{Text: "scored", Data: map[string]any{"points": 5}, Vars: map[string]any{"total": 5}}, nil
	})
	g := Graph{Slug: "s", Entry: "lead",
		Vars: map[string]GraphVar{"notes": {Reducer: ReduceAppend}, "total": {Reducer: ReduceSum}},
		Nodes: []GraphNode{
			agentNode("lead", "lead", "Lead"),
			{ID: "x", Kind: NodeAgent, Agent: &FlowAgent{AgentSlug: "x", OutputKey: "X"}, Save: map[string]string{"notes": "inputs.X"}},
			{ID: "y", Kind: NodeAgent, Agent: &FlowAgent{AgentSlug: "y", OutputKey: "Y"}, Save: map[string]string{"notes": "inputs.Y"}},
			{ID: "score", Kind: NodeFunc, Func: "score", OutputKey: "Score", Save: map[string]string{"total": "data.Score.points * 2"}},
		},
		Edges: []GraphEdge{{From: "lead", To: to("x", "y")}, {From: "x", To: to("score")}, {From: "y", To: to("score")}},
	}
	s := newSession(t, e, map[string]any{"total": 1})
	s.State.Snapshot = []byte(`{"sheet":1}`)
	run, err := e.RunGraph(ctx, s, GraphRequest{Graph: g})
	if err != nil {
		t.Fatal(err)
	}
	if run.Errors["score"] != nil || string(s.State.Snapshot) != `{"sheet":1}` {
		t.Fatalf("score error %v, session snapshot %q", run.Errors["score"], s.State.Snapshot)
	}
	stored, err := e.Sessions().Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	notes := fmt.Sprint(stored.State.Vars["notes"])
	if notes != "[X Y]" && notes != "[Y X]" {
		t.Fatalf("notes = %v, want both branches appended", notes)
	}
	// 1 + 5 (function vars) + 10 (save: 5 * 2), all through the sum reducer.
	if total := toFloat(stored.State.Vars["total"]); total != 16 {
		t.Fatalf("total = %v, want 16", total)
	}
}

// Failure policies: an error edge, Continue, and a required node.
func TestRunGraph_Failures(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "graph_fail")
	mkGenAgent(t, e, "lead", "echo", "L", nil)
	mkGenAgent(t, e, "broken", "fail", "x", nil)
	mkGenAgent(t, e, "next", "echo", "NEXT[{{.Inputs.Broken}}]", []string{"Broken"})
	mkGenAgent(t, e, "apology", "echo", "SORRY", nil)
	mkGenAgent(t, e, "panicky", "panic", "x", nil)

	base := func(broken GraphNode) Graph {
		return Graph{Slug: "f", Entry: "lead",
			Nodes: []GraphNode{agentNode("lead", "lead", "Lead"), broken, agentNode("next", "next", "Next"), agentNode("apology", "apology", "Apology")},
			Edges: []GraphEdge{
				{From: "lead", To: to("broken")},
				{From: "broken", To: to("next")},
				{From: "broken", OnError: true, To: to("apology")},
			}}
	}

	t.Run("error edge", func(t *testing.T) {
		run, err := e.RunGraph(ctx, newSession(t, e, nil), GraphRequest{Graph: base(agentNode("broken", "broken", "Broken"))})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := run.Nodes["apology"]; !ok || !slices.Equal(run.Skipped, []string{"next"}) {
			t.Fatalf("nodes = %v, skipped = %v", run.Nodes, run.Skipped)
		}
		if run.Errors["broken"] == nil {
			t.Fatal("broken's error was not recorded")
		}
	})

	t.Run("continue runs consumers, which fail on the missing input", func(t *testing.T) {
		n := agentNode("broken", "broken", "Broken")
		n.Continue = true
		run, err := e.RunGraph(ctx, newSession(t, e, nil), GraphRequest{Graph: base(n)})
		if err != nil {
			t.Fatal(err)
		}
		if !errors.Is(run.Errors["next"], ErrMissingVariables) {
			t.Fatalf("next error = %v, want ErrMissingVariables", run.Errors["next"])
		}
		if _, ok := run.Nodes["apology"]; !ok {
			t.Fatal("the error edge did not fire alongside Continue")
		}
	})

	t.Run("required fails the run", func(t *testing.T) {
		n := agentNode("broken", "broken", "Broken")
		n.Required = true
		run, err := e.RunGraph(ctx, newSession(t, e, nil), GraphRequest{Graph: base(n)})
		if err == nil || !strings.Contains(err.Error(), `node "broken"`) {
			t.Fatalf("err = %v", err)
		}
		if run == nil || len(run.Steps) != 1 {
			t.Fatalf("want the partial run with the lead's step, got %+v", run)
		}
	})

	t.Run("a panicking node fails alone", func(t *testing.T) {
		g := Graph{Slug: "p", Entry: "lead",
			Nodes: []GraphNode{agentNode("lead", "lead", "Lead"), agentNode("boom", "panicky", ""), agentNode("apology", "apology", "")},
			Edges: []GraphEdge{{From: "lead", To: to("boom", "apology")}}}
		run, err := e.RunGraph(ctx, newSession(t, e, nil), GraphRequest{Graph: g})
		if err != nil {
			t.Fatal(err)
		}
		if run.Errors["boom"] == nil || !strings.Contains(run.Errors["boom"].Error(), "panicked") {
			t.Fatalf("boom error = %v", run.Errors["boom"])
		}
		if _, ok := run.Nodes["apology"]; !ok {
			t.Fatal("the sibling did not run")
		}
	})
}

// A function recorded under an agent becomes a step, like fd-settle.
func TestRunGraph_RecordedFunction(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "graph_record")
	mkGenAgent(t, e, "oddsmaker", "echo", "PREVIEW", nil)
	mkGenAgent(t, e, "fd-settle", "echo", "unused", nil)
	e.RegisterFunc("play-match", func(context.Context, FuncCall) (FuncResult, error) {
		return FuncResult{Text: "Full time: 2-1", Route: "home"}, nil
	})
	g := Graph{Slug: "desk", Entry: "odds",
		Nodes: []GraphNode{agentNode("odds", "oddsmaker", "Preview"), {ID: "settle", Kind: NodeFunc, Func: "play-match", Record: "fd-settle", OutputKey: "Result"}},
		Edges: []GraphEdge{{From: "odds", To: to("settle")}}}
	run, err := e.RunGraph(ctx, newSession(t, e, nil), GraphRequest{Graph: g})
	if err != nil {
		t.Fatal(err)
	}
	st := run.Nodes["settle"]
	if st == nil || ResultText(st.Result) != "Full time: 2-1" || st.Author != "func:play-match" {
		t.Fatalf("settle step = %+v", st)
	}
	if len(run.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(run.Steps))
	}
}

// Graphs that cannot run are refused before anything runs.
func TestRunGraph_RefusesBadGraphs(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "graph_bad")
	mkGenAgent(t, e, "a", "echo", "A", nil)
	mkGenAgent(t, e, "b", "echo", "B[{{.Inputs.C}}]", []string{"C"})
	mkGenAgent(t, e, "c", "echo", "C[{{.Inputs.B}}]", []string{"B"})
	ok := func() Graph {
		return Graph{Slug: "x", Entry: "a", Nodes: []GraphNode{agentNode("a", "a", "A"), agentNode("b", "b", "B")},
			Edges: []GraphEdge{{From: "a", To: to("b")}}}
	}
	cases := map[string]func(g *Graph){
		"missing entry":        func(g *Graph) { g.Entry = "nope" },
		"duplicate id":         func(g *Graph) { g.Nodes[1].ID = "a" },
		"edge to nowhere":      func(g *Graph) { g.Edges[0].To = to("nope") },
		"unreached node":       func(g *Graph) { g.Edges = nil },
		"two triggers":         func(g *Graph) { g.Edges[0].When, g.Edges[0].Default = "true", true },
		"route from an agent":  func(g *Graph) { g.Edges[0].Route = "x" },
		"uncertain off decide": func(g *Graph) { g.Edges[0].Uncertain = 0.5 },
		"bad expression":       func(g *Graph) { g.Edges[0].When = "vars.x >" },
		"unknown name":         func(g *Graph) { g.Edges[0].When = "secrets.key == 1" },
		"unknown function":     func(g *Graph) { g.Nodes = append(g.Nodes, GraphNode{ID: "f", Kind: NodeFunc, Func: "nope"}) },
		"edge back to entry":   func(g *Graph) { g.Edges = append(g.Edges, GraphEdge{From: "b", To: to("a")}) },
		"data cycle": func(g *Graph) {
			g.Nodes = append(g.Nodes, agentNode("c", "c", "C"))
			g.Edges = append(g.Edges, GraphEdge{From: "a", To: to("c")})
		},
		"same output twice": func(g *Graph) { g.Nodes[1].Agent.OutputKey = "A" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			g := ok()
			mutate(&g)
			_, err := e.RunGraph(ctx, newSession(t, e, nil), GraphRequest{Graph: g})
			if !errors.Is(err, ErrInvalidGraph) {
				t.Fatalf("err = %v, want ErrInvalidGraph", err)
			}
		})
	}
	if _, err := e.RunGraph(ctx, newSession(t, e, nil), GraphRequest{Graph: ok()}); err != nil {
		t.Fatalf("the valid graph was refused: %v", err)
	}
}
