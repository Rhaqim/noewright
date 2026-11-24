package engine

// graph_wait_test.go — phase 3: wait nodes, pausing and resuming across calls,
// the cursor in session state and checkpoints, forks, and the input checks.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/rhaqim/noewright/evaluator"
	"github.com/rhaqim/noewright/evaluator/stub"
)

func say(text string) *Action {
	return &Action{Kind: ActionFreeText, Payload: map[string]any{"text": text}}
}

func reload(t *testing.T, e *Engine, s *Session) *Session {
	t.Helper()
	got, err := e.Sessions().Get(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// An uncertain decision pauses to ask; the answer resumes the run, which
// decides again on it and routes.
func TestWait_ClarifyLoop(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "wait_clarify")
	mkGenAgent(t, e, "refund-policy", "echo", "POLICY[{{.Inputs.action}}]", nil)
	g := Graph{Slug: "support", Entry: "intent",
		Nodes: []GraphNode{
			{ID: "intent", Kind: NodeDecide, OutputKey: "Intent", Decide: &GraphDecision{
				Question: EvalChoice("What do they want?", map[string]any{"refund": nil, "bug": nil})}},
			{ID: "clarify", Kind: NodeWait, Wait: &GraphWait{Expect: ExpectText, Prompt: "Do you want a refund or to report a bug?"}},
			agentNode("refund", "refund-policy", "Policy"),
		},
		Edges: []GraphEdge{
			{From: "intent", Uncertain: 0.6, To: to("clarify")},
			{From: "intent", Route: "refund", To: to("refund")},
			{From: "clarify", To: to("intent")},
		}}

	e.SetEvaluator(stub.New().WithAnswer("intent", evaluator.Answer{Type: evaluator.TypeChoice, Choice: "refund", Confidence: 0.3}))
	s := newSession(t, e, nil)
	first, err := e.RunGraph(ctx, s, GraphRequest{Graph: g, Action: say("it's broken and I'm annoyed")})
	if err != nil {
		t.Fatal(err)
	}
	if first.Waiting != "clarify" || first.Prompt == "" || len(first.Steps) != 0 {
		t.Fatalf("first run: waiting %q, prompt %q, %d steps", first.Waiting, first.Prompt, len(first.Steps))
	}
	if cur := reload(t, e, s).State.Graph; cur == nil || cur.Node != "clarify" || cur.Graph != "support" || cur.Visits["intent"] != 1 {
		t.Fatalf("stored cursor = %+v", cur)
	}

	e.SetEvaluator(stub.New().WithChoice("intent", "refund"))
	second, err := e.RunGraph(ctx, s, GraphRequest{Graph: g, Action: say("refund please")})
	if err != nil {
		t.Fatal(err)
	}
	if second.Resumed != "clarify" || second.Waiting != "" {
		t.Fatalf("second run: resumed %q, waiting %q", second.Resumed, second.Waiting)
	}
	if got := ResultText(second.Nodes["refund"].Result); got != "POLICY[refund please]" {
		t.Fatalf("refund = %q; it should see the new action", got)
	}
	if cur := reload(t, e, s).State.Graph; cur != nil {
		t.Fatalf("cursor not cleared after the run finished: %+v", cur)
	}
}

// The human-led tabletop round: the host's narration is a wait node recorded
// as tt-human-dm, and the followers run on it.
func TestWait_HumanLedRound(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "wait_human")
	mkGenAgent(t, e, "tt-human-dm", "echo", "unused", nil)
	mkGenAgent(t, e, "tt-referee", "echo", "REF[{{.Inputs.Narration}}]", []string{"Narration"})
	mkGenAgent(t, e, "tt-chronicler", "echo", "CHRON[{{.Inputs.Narration}}]", []string{"Narration"})
	e.RegisterFunc("collect-actions", func(context.Context, FuncCall) (FuncResult, error) {
		return FuncResult{Text: "Ada: I cross the bridge"}, nil
	})
	g := Graph{Slug: "tabletop-human-round", Entry: "actions",
		Nodes: []GraphNode{
			{ID: "actions", Kind: NodeFunc, Func: "collect-actions", OutputKey: "Actions"},
			{ID: "dm", Kind: NodeWait, OutputKey: "Narration", Wait: &GraphWait{Expect: ExpectText, Record: "tt-human-dm", Prompt: "Narrate the round"}},
			agentNode("referee", "tt-referee", ""), agentNode("chronicler", "tt-chronicler", ""),
		},
		Edges: []GraphEdge{{From: "actions", To: to("dm")}, {From: "dm", To: to("referee", "chronicler")}}}

	s := newSession(t, e, nil)
	if run, err := e.RunGraph(ctx, s, GraphRequest{Graph: g}); err != nil || run.Waiting != "dm" {
		t.Fatalf("first run = %+v, %v", run, err)
	}
	if err := e.AddParticipant(ctx, s, Participant{ID: "host", Role: "host"}); err != nil {
		t.Fatal(err)
	}
	host := say("The bridge groans and gives way.")
	host.ParticipantID = "host"
	run, err := e.RunGraph(ctx, s, GraphRequest{Graph: g, Action: host})
	if err != nil {
		t.Fatal(err)
	}
	dm := run.Nodes["dm"]
	if dm == nil || dm.Author != "participant:host" || ResultText(dm.Result) != "The bridge groans and gives way." {
		t.Fatalf("dm step = %+v, errors %v", dm, run.Errors)
	}
	if got := ResultText(run.Nodes["referee"].Result); got != "REF[The bridge groans and gives way.]" {
		t.Fatalf("referee = %q", got)
	}
	if run.Outputs["Actions"] != "Ada: I cross the bridge" {
		t.Fatalf("outputs from before the pause were lost: %v", run.Outputs)
	}
	// The recorded human step has no cost row; the two followers do.
	if run.Cost.StepCount != 2 {
		t.Fatalf("cost rows = %d, want 2", run.Cost.StepCount)
	}
}

// A fork at the waiting step resumes at the wait, independently of the parent.
func TestWait_ForkResumes(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "wait_fork")
	mkGenAgent(t, e, "a", "echo", "A", nil)
	mkGenAgent(t, e, "b", "echo", "B[{{.Inputs.Answer}}]", []string{"Answer"})
	g := Graph{Slug: "ask", Entry: "a",
		Nodes: []GraphNode{agentNode("a", "a", "A"), {ID: "w", Kind: NodeWait, OutputKey: "Answer"}, agentNode("b", "b", "B")},
		Edges: []GraphEdge{{From: "a", To: to("w")}, {From: "w", To: to("b")}}}
	s := newSession(t, e, map[string]any{"keep": "me"})
	first, err := e.RunGraph(ctx, s, GraphRequest{Graph: g})
	if err != nil || first.Waiting != "w" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	branch, err := e.Sessions().Fork(ctx, s.ID, first.Nodes["a"].Index)
	if err != nil {
		t.Fatal(err)
	}
	if cur := branch.State.Graph; cur == nil || cur.Node != "w" {
		t.Fatalf("branch cursor = %+v, want paused at w", cur)
	}
	onBranch, err := e.RunGraph(ctx, branch, GraphRequest{Graph: g, Action: say("left")})
	if err != nil {
		t.Fatal(err)
	}
	onParent, err := e.RunGraph(ctx, s, GraphRequest{Graph: g, Action: say("right")})
	if err != nil {
		t.Fatal(err)
	}
	if ResultText(onBranch.Nodes["b"].Result) != "B[left]" || ResultText(onParent.Nodes["b"].Result) != "B[right]" {
		t.Fatalf("branch b = %q, parent b = %q", ResultText(onBranch.Nodes["b"].Result), ResultText(onParent.Nodes["b"].Result))
	}
}

// Input checks: a rejected input changes nothing; a choice routes by option.
func TestWait_InputChecksAndChoice(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "wait_choice")
	mkGenAgent(t, e, "start", "echo", "S", nil)
	mkGenAgent(t, e, "yes", "echo", "YES", nil)
	mkGenAgent(t, e, "no", "echo", "NO", nil)
	g := Graph{Slug: "confirm", Entry: "start",
		Nodes: []GraphNode{agentNode("start", "start", "S"),
			{ID: "confirm", Kind: NodeWait, Wait: &GraphWait{Expect: ExpectChoice, Options: []string{"yes", "no"}}},
			agentNode("yes", "yes", "Y"), agentNode("no", "no", "N")},
		Edges: []GraphEdge{{From: "start", To: to("confirm")},
			{From: "confirm", Route: "yes", To: to("yes")}, {From: "confirm", Route: "no", To: to("no")}}}
	s := newSession(t, e, nil)
	if _, err := e.RunGraph(ctx, s, GraphRequest{Graph: g}); err != nil {
		t.Fatal(err)
	}
	before := reload(t, e, s)
	for name, a := range map[string]*Action{
		"no action":     nil,
		"not an option": say("maybe"),
		"bad index":     {Kind: ActionSelect, Payload: map[string]any{"option_index": 7}},
	} {
		if _, err := e.RunGraph(ctx, s, GraphRequest{Graph: g, Action: a}); !errors.Is(err, ErrWaitInput) {
			t.Fatalf("%s: err = %v, want ErrWaitInput", name, err)
		}
	}
	after := reload(t, e, s)
	if after.Version != before.Version || after.State.Graph == nil || after.State.Graph.Node != "confirm" {
		t.Fatalf("a rejected input changed the session: version %d→%d, cursor %+v", before.Version, after.Version, after.State.Graph)
	}
	run, err := e.RunGraph(ctx, s, GraphRequest{Graph: g, Action: &Action{Kind: ActionSelect, Payload: map[string]any{"option_index": float64(1)}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := run.Nodes["no"]; !ok || !slices.Equal(run.Skipped, []string{"yes"}) {
		t.Fatalf("nodes %v, skipped %v; want the no branch", run.Nodes, run.Skipped)
	}
}

// A session paused in one graph refuses another unless asked to restart.
func TestWait_MismatchAndRestart(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "wait_mismatch")
	mkGenAgent(t, e, "a", "echo", "A", nil)
	paused := Graph{Slug: "one", Entry: "a", Nodes: []GraphNode{agentNode("a", "a", "A"), {ID: "w", Kind: NodeWait}},
		Edges: []GraphEdge{{From: "a", To: to("w")}}}
	other := Graph{Slug: "two", Entry: "a", Nodes: []GraphNode{agentNode("a", "a", "A")}}
	s := newSession(t, e, nil)
	if _, err := e.RunGraph(ctx, s, GraphRequest{Graph: paused}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunGraph(ctx, s, GraphRequest{Graph: other}); !errors.Is(err, ErrGraphMismatch) {
		t.Fatalf("err = %v, want ErrGraphMismatch", err)
	}
	run, err := e.RunGraph(ctx, s, GraphRequest{Graph: other, Restart: true})
	if err != nil || len(run.Steps) != 1 {
		t.Fatalf("restart run = %+v, %v", run, err)
	}
	if cur := reload(t, e, s).State.Graph; cur != nil {
		t.Fatalf("cursor after a restarted run that finished = %+v", cur)
	}
}

// MaxVisits caps a loop that passes through a wait.
func TestWait_MaxVisits(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "wait_visits")
	mkGenAgent(t, e, "start", "echo", "S", nil)
	mkGenAgent(t, e, "answer", "echo", "ANSWER[{{.Inputs.Question}}]", []string{"Question"})
	g := Graph{Slug: "qa", Entry: "start",
		Nodes: []GraphNode{agentNode("start", "start", "S"),
			{ID: "ask", Kind: NodeWait, OutputKey: "Question"},
			{ID: "answer", Kind: NodeAgent, Agent: &FlowAgent{AgentSlug: "answer", OutputKey: "Answer"}, MaxVisits: 2},
		},
		Edges: []GraphEdge{{From: "start", To: to("ask")}, {From: "ask", To: to("answer")},
			{From: "answer", To: to("ask"), When: "visits.answer < 2"}}}
	s := newSession(t, e, nil)
	if _, err := e.RunGraph(ctx, s, GraphRequest{Graph: g}); err != nil {
		t.Fatal(err)
	}
	r1, err := e.RunGraph(ctx, s, GraphRequest{Graph: g, Action: say("q1")})
	if err != nil || r1.Waiting != "ask" {
		t.Fatalf("after q1: %+v, %v", r1, err)
	}
	r2, err := e.RunGraph(ctx, s, GraphRequest{Graph: g, Action: say("q2")})
	if err != nil || r2.Waiting != "" || ResultText(r2.Nodes["answer"].Result) != "ANSWER[q2]" {
		t.Fatalf("after q2 the loop should end: waiting %q, %v", r2.Waiting, err)
	}
}

// Concurrent calls on one session run one after the other.
func TestWait_ConcurrentCallsSerialise(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "wait_concurrent")
	mkGenAgent(t, e, "a", "echo", "A", nil)
	mkGenAgent(t, e, "b", "echo", "B", nil)
	g := Graph{Slug: "par", Entry: "a", Nodes: []GraphNode{agentNode("a", "a", "A"), agentNode("b", "b", "B")},
		Edges: []GraphEdge{{From: "a", To: to("b")}}}
	s := newSession(t, e, nil)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.RunGraph(ctx, s, GraphRequest{Graph: g})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	steps, err := e.Sessions().Steps(ctx, s.ID, 100, 0)
	if err != nil || len(steps) != 8 {
		t.Fatalf("steps = %d, %v; want 8", len(steps), err)
	}
}

// Graphs whose waits cannot work are refused.
func TestWait_RefusesBadGraphs(t *testing.T) {
	ctx := context.Background()
	e := graphEngine(t, "wait_bad")
	mkGenAgent(t, e, "a", "echo", "A", nil)
	mkGenAgent(t, e, "b", "echo", "B", nil)
	cases := map[string]Graph{
		"entry is a wait": {Slug: "x", Entry: "w", Nodes: []GraphNode{{ID: "w", Kind: NodeWait}, agentNode("a", "a", "A")},
			Edges: []GraphEdge{{From: "w", To: to("a")}}},
		"choice without options": {Slug: "x", Entry: "a", Nodes: []GraphNode{agentNode("a", "a", "A"), {ID: "w", Kind: NodeWait, Wait: &GraphWait{Expect: ExpectChoice}}},
			Edges: []GraphEdge{{From: "a", To: to("w")}}},
		"bad pattern": {Slug: "x", Entry: "a", Nodes: []GraphNode{agentNode("a", "a", "A"), {ID: "w", Kind: NodeWait, Wait: &GraphWait{Expect: ExpectText, Pattern: "("}}},
			Edges: []GraphEdge{{From: "a", To: to("w")}}},
		"cycle with no wait": {Slug: "x", Entry: "a", Nodes: []GraphNode{agentNode("a", "a", "A"), agentNode("b", "b", "B"), {ID: "c", Kind: NodeFunc, Func: "noop"}},
			Edges: []GraphEdge{{From: "a", To: to("b")}, {From: "b", To: to("c")}, {From: "c", To: to("b")}}},
	}
	e.RegisterFunc("noop", func(context.Context, FuncCall) (FuncResult, error) { return FuncResult{}, nil })
	for name, g := range cases {
		if _, err := e.RunGraph(ctx, newSession(t, e, nil), GraphRequest{Graph: g}); !errors.Is(err, ErrInvalidGraph) {
			t.Errorf("%s: err = %v, want ErrInvalidGraph", name, err)
		}
	}
}
