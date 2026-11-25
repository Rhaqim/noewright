package engine_test

// graph_demos_test.go — the phase 2 gate: the rhaqim.com demos' request shapes
// (tabletop round, sermon captions, forecast desk) each run as one graph,
// through the public API, on SQLite and on Postgres, with no API keys.

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/rhaqim/noewright"
	"github.com/rhaqim/noewright/internal/dsnenv"
	"github.com/rhaqim/noewright/schema"
)

func TestGraphDemosSQLite(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:graphdemo_"+uuid.NewString()+"?mode=memory&cache=shared")
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
	graphDemos(t, e)
}

func TestGraphDemosPostgres(t *testing.T) {
	dsn := dsnenv.Get()
	if dsn == "" {
		t.Skip("set NOEWRIGHT_DSN to run the demo graphs on Postgres")
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
	graphDemos(t, e)
}

// demoKit creates agents and sessions in a fresh owner scope.
type demoKit struct {
	t     *testing.T
	e     *noewright.Engine
	owner string
}

func (k demoKit) agent(slug, body string, vars ...string) {
	k.t.Helper()
	ctx := context.Background()
	ut := &noewright.Prompt{Owner: k.owner, Slug: slug + "-ut", Version: 1, Kind: noewright.PromptKindUserTemplate, Body: body, Variables: vars}
	if err := k.e.Prompts().Create(ctx, ut); err != nil {
		k.t.Fatal(err)
	}
	if err := k.e.Agents().Create(ctx, &noewright.Agent{Owner: k.owner, Slug: slug, Version: 1, Modal: noewright.ModalityText,
		GeneratorSlug: "g", UserTemplateID: ut.ID}); err != nil {
		k.t.Fatal(err)
	}
}

func (k demoKit) session(vars map[string]any) *noewright.Session {
	k.t.Helper()
	s := &noewright.Session{PlatformID: "demo", State: noewright.State{Vars: vars}}
	if err := k.e.Sessions().Create(context.Background(), s); err != nil {
		k.t.Fatal(err)
	}
	return s
}

func agentAt(id, slug, out string) noewright.GraphNode {
	return noewright.GraphNode{ID: id, Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: slug, OutputKey: out}}
}

func edge(from string, to ...string) noewright.GraphEdge {
	return noewright.GraphEdge{From: from, To: to}
}

func text(r *noewright.GraphRun, node string) string {
	if st := r.Nodes[node]; st != nil {
		return noewright.ResultText(st.Result)
	}
	return ""
}

func graphDemos(t *testing.T, e *noewright.Engine) {
	ctx := context.Background()
	k := demoKit{t: t, e: e, owner: "demo-" + uuid.NewString()[:8]}

	t.Run("tabletop round", func(t *testing.T) {
		k.agent("tt-rules", "RULES[{{.Inputs.Actions}}]")
		k.agent("tt-gm", "GM[{{.Inputs.Actions}}|{{.Inputs.Rolls}}]")
		k.agent("tt-referee", "REF[{{.Inputs.Narration}}]", "Narration")
		k.agent("tt-chronicler", "CHRON[{{.Inputs.Narration}}]", "Narration")
		k.agent("tt-suggest", "SUGG[{{.Inputs.Narration}}]", "Narration")
		e.RegisterFunc("tt-start", func(context.Context, noewright.FuncCall) (noewright.FuncResult, error) {
			return noewright.FuncResult{}, nil
		})
		e.RegisterFunc("tt-roll", func(_ context.Context, c noewright.FuncCall) (noewright.FuncResult, error) {
			return noewright.FuncResult{Text: "Ada DEX check: 17 vs DC 12, success"}, nil
		})
		g := noewright.Graph{Slug: "tabletop-round", Entry: "start", Inputs: []string{"Actions", "Rolls"},
			Nodes: []noewright.GraphNode{
				{ID: "start", Kind: noewright.NodeFunc, Func: "tt-start"},
				agentAt("rules", "tt-rules", "Rules"),
				{ID: "roll", Kind: noewright.NodeFunc, Func: "tt-roll", OutputKey: "Rolls"},
				{ID: "gm", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "tt-gm", OutputKey: "Narration", Stream: true}},
				agentAt("referee", "tt-referee", ""), agentAt("chronicler", "tt-chronicler", ""), agentAt("suggest", "tt-suggest", ""),
			},
			Edges: []noewright.GraphEdge{
				{From: "start", To: []string{"rules"}, When: "vars.round > 0"},
				{From: "start", To: []string{"gm"}, Default: true},
				edge("rules", "roll"), edge("roll", "gm"), edge("gm", "referee", "chronicler", "suggest"),
			}}
		if err := e.Graphs().Validate(ctx, k.owner, g); err != nil {
			t.Fatal(err)
		}

		opening, err := e.RunGraph(ctx, k.session(map[string]any{"round": 0}), noewright.GraphRequest{Graph: g, Owner: k.owner,
			Inputs: map[string]any{"Actions": "(the adventure begins)", "Rolls": "No checks this round."}})
		if err != nil {
			t.Fatal(err)
		}
		if got := text(opening, "gm"); got != "GM[(the adventure begins)|No checks this round.]" {
			t.Fatalf("opening gm = %q", got)
		}
		slices.Sort(opening.Skipped)
		if !slices.Equal(opening.Skipped, []string{"roll", "rules"}) || len(opening.Steps) != 4 {
			t.Fatalf("opening skipped %v with %d steps", opening.Skipped, len(opening.Steps))
		}

		round, err := e.RunGraph(ctx, k.session(map[string]any{"round": 2}), noewright.GraphRequest{Graph: g, Owner: k.owner,
			Inputs: map[string]any{"Actions": "Ada picks the lock", "Rolls": "unused"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := text(round, "gm"); got != "GM[Ada picks the lock|Ada DEX check: 17 vs DC 12, success]" {
			t.Fatalf("round gm = %q", got)
		}
		for _, f := range []string{"referee", "chronicler", "suggest"} {
			if !strings.Contains(text(round, f), "GM[Ada picks the lock") {
				t.Fatalf("%s did not see the narration: %q", f, text(round, f))
			}
		}
		// rules, gm and three followers each cost a row; the dice record nothing.
		if round.Cost.StepCount != 5 || round.Cost.TotalTokens != 10 {
			t.Fatalf("round cost = %+v, want 5 rows and 10 tokens", round.Cost)
		}
	})

	t.Run("sermon captions", func(t *testing.T) {
		k.agent("sc-detect", "REFS[{{.Inputs.Line}}]")
		k.agent("sc-caption", "EN[{{.Inputs.Line}}|{{.Inputs.Scripture}}]", "Scripture")
		for _, l := range []string{"yo", "ig", "pcm"} {
			k.agent("sc-"+l, strings.ToUpper(l)+"[{{.Inputs.Caption}}|{{.Inputs.Guard}}]", "Caption")
		}
		k.agent("sc-judge", "JUDGE[{{.Inputs.Yo}}/{{.Inputs.Ig}}/{{.Inputs.Pcm}}]", "Yo", "Ig", "Pcm")
		e.RegisterFunc("kjv-lookup", func(_ context.Context, c noewright.FuncCall) (noewright.FuncResult, error) {
			return noewright.FuncResult{Text: "John 3:16", Data: map[string]any{"refs": 1, "book": "JHN"}}, nil
		})
		g := noewright.Graph{Slug: "sermon-captions", Entry: "detect", Inputs: []string{"Line"},
			Nodes: []noewright.GraphNode{
				agentAt("detect", "sc-detect", "Refs"),
				{ID: "scripture", Kind: noewright.NodeFunc, Func: "kjv-lookup", OutputKey: "Scripture"},
				{ID: "caption", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "sc-caption", OutputKey: "Caption", Stream: true}},
				{ID: "yo", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "sc-yo", OutputKey: "Yo", Inputs: map[string]any{"Guard": "keep Johanu"}}},
				{ID: "ig", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "sc-ig", OutputKey: "Ig", Inputs: map[string]any{"Guard": "keep Jọn"}}},
				{ID: "pcm", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "sc-pcm", OutputKey: "Pcm", Inputs: map[string]any{"Guard": "keep John"}}},
				agentAt("judge", "sc-judge", "Judge"),
			},
			Edges: []noewright.GraphEdge{
				edge("detect", "scripture"), edge("scripture", "caption"), edge("caption", "yo", "ig", "pcm"),
				edge("yo", "judge"), edge("ig", "judge"), edge("pcm", "judge"),
			}}
		if err := e.Graphs().Validate(ctx, k.owner, g); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var done []string
		run, err := e.RunGraph(ctx, k.session(nil), noewright.GraphRequest{Graph: g, Owner: k.owner,
			Inputs: map[string]any{"Line": "For God so loved the world"},
			OnNode: func(ev noewright.NodeEvent) {
				if ev.Kind == noewright.NodeDone {
					mu.Lock()
					done = append(done, ev.Node)
					mu.Unlock()
				}
			}})
		if err != nil {
			t.Fatal(err)
		}
		want := "EN[For God so loved the world|John 3:16]"
		if got := text(run, "caption"); got != want {
			t.Fatalf("caption = %q", got)
		}
		if got := text(run, "yo"); got != "YO["+want+"|keep Johanu]" {
			t.Fatalf("yo = %q", got)
		}
		if got := text(run, "judge"); !strings.HasPrefix(got, "JUDGE[YO[") || !strings.Contains(got, "/PCM[") {
			t.Fatalf("judge did not see all three translations: %q", got)
		}
		if done[len(done)-1] != "judge" || len(run.Steps) != 6 {
			t.Fatalf("done order %v, %d steps", done, len(run.Steps))
		}
		if run.Cost.PricingConfigured {
			t.Fatal("pricing reported as configured with no Config.Pricing")
		}
	})

	t.Run("forecast desk", func(t *testing.T) {
		k.agent("fd-oddsmaker", "PREVIEW[{{.Inputs.Card}}]")
		k.agent("fd-value", "VALUE[{{.Inputs.Preview}}]", "Preview")
		k.agent("fd-contrarian", "CONTRA[{{.Inputs.Preview}}]", "Preview")
		k.agent("fd-bankroll", "STAKES[{{.Inputs.ValuePick}}+{{.Inputs.ContrarianPick}}]", "ValuePick", "ContrarianPick")
		k.agent("fd-settle", "unused")
		e.RegisterFunc("play-match", func(_ context.Context, c noewright.FuncCall) (noewright.FuncResult, error) {
			chips := c.Vars["chips"]
			return noewright.FuncResult{Text: fmt.Sprintf("Full time: Lagos 2-1 Accra (chips were %v)", chips),
				Route: "home", Vars: map[string]any{"chips": -10}}, nil
		})
		g := noewright.Graph{Slug: "forecast-match", Entry: "odds", Inputs: []string{"Card"},
			Vars: map[string]noewright.GraphVar{"chips": {Reducer: noewright.ReduceSum}},
			Nodes: []noewright.GraphNode{
				{ID: "odds", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "fd-oddsmaker", OutputKey: "Preview", Stream: true}},
				agentAt("value", "fd-value", "ValuePick"),
				agentAt("contrarian", "fd-contrarian", "ContrarianPick"),
				agentAt("bankroll", "fd-bankroll", "Stakes"),
				{ID: "settle", Kind: noewright.NodeFunc, Func: "play-match", Record: "fd-settle", OutputKey: "Result"},
			},
			Edges: []noewright.GraphEdge{
				edge("odds", "value", "contrarian"), edge("value", "bankroll"), edge("contrarian", "bankroll"), edge("bankroll", "settle"),
			}}
		if err := e.Graphs().Validate(ctx, k.owner, g); err != nil {
			t.Fatal(err)
		}
		sess := k.session(map[string]any{"chips": 100})
		run, err := e.RunGraph(ctx, sess, noewright.GraphRequest{Graph: g, Owner: k.owner, Inputs: map[string]any{"Card": "Lagos v Accra"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := text(run, "bankroll"); got != "STAKES[VALUE[PREVIEW[Lagos v Accra]]+CONTRA[PREVIEW[Lagos v Accra]]]" {
			t.Fatalf("bankroll = %q", got)
		}
		settle := run.Nodes["settle"]
		if settle == nil || settle.Author != "func:play-match" || !strings.Contains(noewright.ResultText(settle.Result), "chips were 100") {
			t.Fatalf("settle step = %+v", settle)
		}
		if run.Cost.StepCount != len(run.Steps)-1 {
			t.Fatalf("cost rows = %d for %d steps; the recorded settle step has none", run.Cost.StepCount, len(run.Steps))
		}
		stored, err := e.Sessions().Get(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if chips := fmt.Sprint(stored.State.Vars["chips"]); chips != "90" {
			t.Fatalf("chips = %s, want 90", chips)
		}
	})
}
