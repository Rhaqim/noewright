package engine_test

// graph_store_test.go — saved graphs on both dialects, through the public
// facade: save, read back (including an Evaluator question), version
// resolution, run by slug, and the registry-level checks and plan.

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rhaqim/noewright"
	"github.com/rhaqim/noewright/evaluator/stub"
	"github.com/rhaqim/noewright/internal/dsnenv"
	"github.com/rhaqim/noewright/schema"
)

type templateGen struct{}

func (templateGen) Modality() noewright.Modality { return noewright.ModalityText }
func (templateGen) Generate(_ context.Context, req noewright.GenerateRequest) (noewright.Result, error) {
	return noewright.NewTextResult(req.UserPrompt, "stop", 1, 1), nil
}

func TestGraphStoreSQLite(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:graphrt_"+uuid.NewString()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	e, err := noewright.New(noewright.Config{DB: db, Dialect: noewright.DialectSQLite,
		Generators: map[string]noewright.Generator{"g": templateGen{}}})
	if err != nil {
		t.Fatal(err)
	}
	graphStore(t, e)
}

func TestGraphStorePostgres(t *testing.T) {
	dsn := dsnenv.Get()
	if dsn == "" {
		t.Skip("set NOEWRIGHT_DSN to run the Postgres graph round-trip")
	}
	ctx := context.Background()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := schema.NewLoader(schema.DialectPostgres).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	e, err := noewright.New(noewright.Config{DB: db, Dialect: noewright.DialectPostgres,
		Generators: map[string]noewright.Generator{"g": templateGen{}}})
	if err != nil {
		t.Fatal(err)
	}
	graphStore(t, e)
}

func graphStore(t *testing.T, e *noewright.Engine) {
	t.Helper()
	ctx := context.Background()
	owner := "tenant-" + uuid.NewString()[:8]
	agent := func(slug, body string, vars ...string) {
		ut := &noewright.Prompt{Owner: owner, Slug: slug + "-ut", Version: 1, Kind: noewright.PromptKindUserTemplate, Body: body, Variables: vars}
		if err := e.Prompts().Create(ctx, ut); err != nil {
			t.Fatal(err)
		}
		if err := e.Agents().Create(ctx, &noewright.Agent{Owner: owner, Slug: slug, Version: 1, Modal: noewright.ModalityText,
			GeneratorSlug: "g", UserTemplateID: ut.ID}); err != nil {
			t.Fatal(err)
		}
	}
	agent("triage", "T[{{.Inputs.action}}]")
	agent("refund", "REFUND[{{.Inputs.Ticket}}]", "Ticket")
	agent("search", "SEARCH[{{.Inputs.Ticket}}]", "Ticket")
	e.SetEvaluator(stub.New().WithChoice("intent", "refund"))

	g := noewright.Graph{
		Entry: "intake",
		Vars:  map[string]noewright.GraphVar{"seen": {Reducer: noewright.ReduceAppend}},
		Nodes: []noewright.GraphNode{
			{ID: "intake", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "triage", OutputKey: "Ticket"}},
			{ID: "intent", Kind: noewright.NodeDecide, OutputKey: "Intent", Decide: &noewright.GraphDecision{
				Question: noewright.EvalChoice("What do they want?", map[string]any{"refund": "money back", "question": "help"}),
				Over:     "Ticket"}},
			{ID: "refund", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "refund", OutputKey: "Refund"},
				Save: map[string]string{"seen": "'refund'"}},
			{ID: "search", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "search", OutputKey: "Search"}},
		},
		Edges: []noewright.GraphEdge{
			{From: "intake", To: []string{"intent"}},
			{From: "intent", Route: "refund", To: []string{"refund"}},
			{From: "intent", Default: true, To: []string{"search"}},
		},
	}

	// Save v1 active and v2 as a draft; version 0 resolves to v1.
	if err := e.Graphs().Create(ctx, &noewright.GraphRecord{Owner: owner, Slug: "support", Version: 1, IsActive: true, Graph: g}); err != nil {
		t.Fatal(err)
	}
	draft := g
	draft.Edges = slices.Clone(g.Edges)
	draft.Edges[1] = noewright.GraphEdge{From: "intent", Route: "question", To: []string{"search"}}
	draft.Edges[2] = noewright.GraphEdge{From: "intent", Default: true, To: []string{"refund"}}
	if err := e.Graphs().Create(ctx, &noewright.GraphRecord{Owner: owner, Slug: "support", Version: 2, Graph: draft}); err != nil {
		t.Fatal(err)
	}
	got, err := e.Graphs().Get(ctx, owner, "support", 0)
	if err != nil || got.Version != 1 {
		t.Fatalf("version 0 resolved to %+v, %v; want v1 (v2 is a draft)", got, err)
	}
	if latest, _ := e.Graphs().Latest(ctx, owner, "support"); latest == nil || latest.Version != 2 {
		t.Fatalf("Latest = %+v, want the v2 draft", latest)
	}
	// The question survived the round trip in a shape Validate accepts.
	if err := got.Graph.Nodes[1].Decide.Question.Validate(); err != nil {
		t.Fatalf("decoded question is invalid: %v", err)
	}
	if got.Graph.Slug != "support" || got.Graph.Vars["seen"].Reducer != noewright.ReduceAppend {
		t.Fatalf("decoded graph = %+v", got.Graph)
	}

	// Run the active version by slug.
	sess := &noewright.Session{PlatformID: "p"}
	if err := e.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	run, err := e.RunGraphBySlug(ctx, sess, owner, "support", 0, noewright.GraphRequest{
		Action: &noewright.Action{Kind: noewright.ActionFreeText, Payload: map[string]any{"text": "money back please"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := noewright.ResultText(run.Nodes["refund"].Result); got != "REFUND[T[money back please]]" {
		t.Fatalf("refund = %q", got)
	}
	if !slices.Equal(run.Skipped, []string{"search"}) {
		t.Fatalf("skipped = %v", run.Skipped)
	}
	stored, err := e.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if seen, _ := stored.State.Vars["seen"].([]any); len(seen) != 1 || seen[0] != "refund" {
		t.Fatalf("seen = %#v", stored.State.Vars["seen"])
	}

	// Publish v2, then roll back to v1.
	if err := e.Graphs().SetActive(ctx, owner, "support", 2); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Graphs().Get(ctx, owner, "support", 0); got == nil || got.Version != 2 {
		t.Fatalf("after SetActive(2), version 0 = %+v", got)
	}
	if err := e.Graphs().SetActive(ctx, owner, "support", 1); err != nil {
		t.Fatal(err)
	}
	var nf *noewright.NotFoundError
	if err := e.Graphs().SetActive(ctx, owner, "support", 9); !errors.As(err, &nf) {
		t.Fatalf("SetActive(9) = %v, want NotFoundError", err)
	}
	if list, err := e.Graphs().List(ctx, owner, ""); err != nil || len(list) != 2 {
		t.Fatalf("List = %d records, %v", len(list), err)
	}

	// Overrides carry secrets and are never saved.
	leaky := g
	leaky.Nodes = slices.Clone(g.Nodes)
	leaky.Nodes[0].Agent = &noewright.FlowAgent{AgentSlug: "triage", Overrides: map[string]any{"api_key": "sk-secret"}}
	if err := e.Graphs().Create(ctx, &noewright.GraphRecord{Owner: owner, Slug: "leaky", Version: 1, Graph: leaky}); !errors.Is(err, noewright.ErrInvalidGraph) {
		t.Fatalf("saving overrides = %v, want ErrInvalidGraph", err)
	}

	// Validate: a missing agent, an input nothing produces, and two parallel
	// saves of a var with no reducer.
	bad := noewright.Graph{Entry: "a",
		Nodes: []noewright.GraphNode{
			{ID: "a", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "triage", OutputKey: "A"}},
			{ID: "b", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "nope"}, Save: map[string]string{"x": "1"}},
			{ID: "c", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "refund"}, Save: map[string]string{"x": "2"}},
		},
		Edges: []noewright.GraphEdge{{From: "a", To: []string{"b", "c"}}}}
	err = e.Graphs().Validate(ctx, owner, bad)
	if !errors.Is(err, noewright.ErrInvalidGraph) {
		t.Fatalf("Validate = %v, want ErrInvalidGraph", err)
	}
	for _, want := range []string{`agent "nope"`, `reads "Ticket"`, `both save "x"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate error lacks %q: %v", want, err)
		}
	}
	if err := e.Graphs().Validate(ctx, owner, g); err != nil {
		t.Fatalf("Validate(valid) = %v", err)
	}

	// Plan draws the graph, marks data edges, and lists problems instead of
	// refusing.
	plan, err := e.Graphs().Plan(ctx, owner, g)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Problems) != 0 || len(plan.Nodes) != 4 {
		t.Fatalf("plan = %+v", plan)
	}
	var data []string
	for _, ed := range plan.Edges {
		if ed.Data {
			data = append(data, ed.From+">"+ed.To+":"+ed.Var)
		}
	}
	slices.Sort(data)
	if !slices.Equal(data, []string{"intake>refund:Ticket", "intake>search:Ticket"}) {
		t.Fatalf("data edges = %v", data)
	}
	badPlan, err := e.Graphs().Plan(ctx, owner, bad)
	if err != nil || len(badPlan.Problems) == 0 {
		t.Fatalf("plan of a bad graph = %+v, %v; want problems", badPlan, err)
	}

	if err := e.Graphs().Delete(ctx, owner, "support", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Graphs().Get(ctx, owner, "support", 2); !errors.As(err, &nf) {
		t.Fatalf("Get after delete = %v", err)
	}
}
