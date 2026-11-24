package engine

// graph_turn_diff_test.go — differential test: the graph-backed RunTurn must
// be indistinguishable from the pre-graph implementation (runTurnLegacy) to a
// caller. Each scenario runs the same flow through both, on fresh sessions of
// one engine, and compares everything a caller can observe.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

type downGen struct{}

func (downGen) Modality() Modality { return ModalityText }
func (downGen) Generate(context.Context, GenerateRequest) (Result, error) {
	return nil, errors.New("provider down")
}

type panicGen struct{}

func (panicGen) Modality() Modality { return ModalityText }
func (panicGen) Generate(context.Context, GenerateRequest) (Result, error) {
	panic("boom")
}

// mkGenAgent is mkEchoAgent with a chosen generator.
func mkGenAgent(t *testing.T, e *Engine, slug, gen, body string, vars []string) {
	t.Helper()
	ctx := context.Background()
	ut := &Prompt{Slug: slug + "-ut", Version: 1, Kind: PromptKindUserTemplate, Body: body, Variables: vars}
	if err := e.Prompts().Create(ctx, ut); err != nil {
		t.Fatal(err)
	}
	if err := e.Agents().Create(ctx, &Agent{
		Slug: slug, Version: 1, Modal: ModalityText, GeneratorSlug: gen, UserTemplateID: ut.ID,
	}); err != nil {
		t.Fatal(err)
	}
}

// turnView is everything a caller can observe from one RunTurn call, in a
// form that compares equal across runs (no IDs, timestamps or nondeterministic
// completion order).
type turnView struct {
	Err       string
	Lead      string
	Followers map[string]string
	Errors    map[string]string
	Skipped   []string // sorted: completion order is nondeterministic
	Steps     int
	Roles     []string // turn_role of each step, sorted
	FirstCall string   // the first OnStep role
	Calls     []string // every OnStep call as role|step?|err, sorted
	Bus       []string // bus topics published, sorted
}

func viewTurn(turn *Turn, err error, calls []string) turnView {
	v := turnView{Followers: map[string]string{}, Errors: map[string]string{}}
	if err != nil {
		// Strip the session-specific parts; keep the error's shape.
		v.Err = err.Error()
	}
	if len(calls) > 0 {
		v.FirstCall = strings.SplitN(calls[0], "|", 2)[0]
	}
	v.Calls = slices.Clone(calls)
	sort.Strings(v.Calls)
	if turn == nil {
		return v
	}
	v.Lead = ResultText(turn.Lead.Result)
	for k, s := range turn.Followers {
		v.Followers[k] = ResultText(s.Result)
	}
	for k, e := range turn.Errors {
		v.Errors[k] = e.Error()
	}
	v.Skipped = slices.Clone(turn.Skipped)
	sort.Strings(v.Skipped)
	v.Steps = len(turn.Steps)
	for _, s := range turn.Steps {
		v.Roles = append(v.Roles, fmt.Sprint(s.Diagnostics["turn_role"]))
	}
	sort.Strings(v.Roles)
	for _, m := range turn.Messages {
		v.Bus = append(v.Bus, m.Topic)
	}
	sort.Strings(v.Bus)
	return v
}

func TestRunTurnMatchesLegacy(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "turn_diff", map[string]Generator{
		"echo": echoPromptGen{}, "fail": downGen{}, "panic": panicGen{},
	}, PollerConfig{})

	mkGenAgent(t, e, "lead", "echo", "LEAD:{{.Inputs.action}}", nil)
	mkGenAgent(t, e, "lead-fail", "fail", "x", nil)
	mkGenAgent(t, e, "a", "echo", "A<{{.Inputs.Lead}}>", []string{"Lead"})
	mkGenAgent(t, e, "b", "echo", "B<{{.Inputs.A}}>", []string{"A"})
	mkGenAgent(t, e, "c", "echo", "C<{{.Inputs.A}}>", []string{"A"})
	mkGenAgent(t, e, "d", "echo", "D<{{.Inputs.B}}|{{.Inputs.C}}>", []string{"B", "C"})
	mkGenAgent(t, e, "solo", "echo", "SOLO", nil)
	mkGenAgent(t, e, "broken", "fail", "x", nil)
	mkGenAgent(t, e, "after-broken", "echo", "AB<{{.Inputs.Broken}}>", []string{"Broken"})
	mkGenAgent(t, e, "panicky", "panic", "x", nil)
	mkGenAgent(t, e, "cyc1", "echo", "{{.Inputs.Cyc2}}", []string{"Cyc2"})
	mkGenAgent(t, e, "cyc2", "echo", "{{.Inputs.Cyc1}}", []string{"Cyc1"})

	// Requests are built fresh for every run: the engine assigns IDs to the
	// Action and to a LeadResult when it persists them, so sharing one object
	// between runs would collide on the second insert.
	var action *Action
	newAction := func() *Action { return &Action{Kind: ActionFreeText, Payload: map[string]any{"text": "open the door"}} }
	var hostText Result
	gateNo := func(context.Context, FollowerGate) (bool, error) { return false, nil }
	gateErr := func(context.Context, FollowerGate) (bool, error) { return false, errors.New("cannot decide") }
	gateLead := func(_ context.Context, g FollowerGate) (bool, error) {
		return strings.Contains(ResultText(g.Lead.Result), "door"), nil
	}

	type scenario struct {
		name string
		req  TurnRequest
	}
	scenarios := func() []scenario {
		return []scenario{
			{"lead only", TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"}}, Action: action}},
			{"diamond layers", TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"}, Followers: []FlowAgent{
				{AgentSlug: "d", OutputKey: "D"}, {AgentSlug: "b", OutputKey: "B"}, {AgentSlug: "a", OutputKey: "A"},
				{AgentSlug: "c", OutputKey: "C"}, {AgentSlug: "solo"},
			}}, Action: action}},
			{"streaming lead", TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead", Stream: true, OutputKey: "Lead"},
				Followers: []FlowAgent{{AgentSlug: "a", OutputKey: "A"}}}, Action: action, OnChunk: func(Chunk) {}}},
			{"failed follower feeds a consumer", TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"}, Followers: []FlowAgent{
				{AgentSlug: "broken", OutputKey: "Broken"}, {AgentSlug: "after-broken", OutputKey: "AB"}, {AgentSlug: "solo"},
			}}, Action: action}},
			{"panicking follower", TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"}, Followers: []FlowAgent{
				{AgentSlug: "panicky"}, {AgentSlug: "solo"},
			}}, Action: action}},
			{"gates", TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"}, Followers: []FlowAgent{
				{AgentSlug: "a", OutputKey: "A", When: gateLead}, {AgentSlug: "b", OutputKey: "B", When: gateNo},
				{AgentSlug: "solo", When: gateErr}, {AgentSlug: "c", OutputKey: "C"},
			}}, Action: action}},
			{"lead fails", TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead-fail"}, Followers: []FlowAgent{
				{AgentSlug: "solo"},
			}}, Action: action}},
			{"missing lead agent", TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "nope"}}, Action: action}},
			{"human-led", TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"}, Followers: []FlowAgent{
				{AgentSlug: "a", OutputKey: "A"},
			}}, LeadResult: hostText, LeadAuthor: "participant:host"}},
			{"human-led without author", TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"}},
				LeadResult: NewTextResult("x", "stop", 0, 0)}},
			{"cycle", TurnRequest{Flow: Flow{Slug: "t", Lead: FlowAgent{AgentSlug: "lead"}, Followers: []FlowAgent{
				{AgentSlug: "cyc1", OutputKey: "Cyc1"}, {AgentSlug: "cyc2", OutputKey: "Cyc2"}, {AgentSlug: "solo"},
			}}, Action: action}},
			{"per-agent inputs and params", TurnRequest{Flow: Flow{Slug: "t",
				Lead:      FlowAgent{AgentSlug: "lead", Inputs: map[string]any{"action": "lead-only action"}},
				Followers: []FlowAgent{{AgentSlug: "a", OutputKey: "A", Params: map[string]any{"temperature": 0.2}}},
			}, Action: action, Params: map[string]any{"temperature": 0.9}}},
		}
	}
	fresh := func(i int) TurnRequest {
		action = newAction()
		hostText = NewTextResult("the host speaks", "stop", 0, 0)
		return scenarios()[i].req
	}

	run := func(legacy bool, req TurnRequest) turnView {
		sess := &Session{PlatformID: "p"}
		if err := e.Sessions().Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var calls []string
		req.OnStep = func(role string, step *Step, err error) {
			mu.Lock()
			defer mu.Unlock()
			es := ""
			if err != nil {
				es = err.Error()
			}
			calls = append(calls, fmt.Sprintf("%s|%v|%s", role, step != nil, es))
		}
		var turn *Turn
		var err error
		if legacy {
			turn, err = e.runTurnLegacy(ctx, sess, req)
		} else {
			turn, err = e.RunTurn(ctx, sess, req)
		}
		return viewTurn(turn, err, calls)
	}

	for i, sc := range scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			// Run each side a few times: completion order varies between runs,
			// and the views must match regardless.
			for range 3 {
				want := run(true, fresh(i))
				got := run(false, fresh(i))
				if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", want) {
					t.Fatalf("graph RunTurn differs from legacy\n got: %#v\nwant: %#v", got, want)
				}
			}
		})
	}
}
