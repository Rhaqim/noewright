// Quickstart: how to use noewright to run a multi-agent turn — a lead that "writes"
// and a follower that "reviews" it — with NO API key and NO external database
// (in-memory SQLite + a tiny local generator). Run: go run ./examples/quickstart
package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/rhaqim/noewright"
	"github.com/rhaqim/noewright/schema"
	_ "modernc.org/sqlite"
)

// storyGen stands in for "your model". noewright hands it the rendered system + user
// prompt and it returns a result. Swap this for generator/openai or
// generator/anthropic and nothing else in this file changes.
type storyGen struct{}

func (storyGen) Modality() noewright.Modality { return noewright.ModalityText }

func (storyGen) Generate(_ context.Context, req noewright.GenerateRequest) (noewright.Result, error) {
	if strings.Contains(req.SystemPrompt, "critic") {
		// The follower received the lead's prose via {{.Inputs.Lead}}.
		return noewright.NewTextResult("Strong hook — trim one adjective and it sings. 8/10.", "stop", 0, 0), nil
	}
	return noewright.NewTextResult("You shoulder the creaking door open; cold air and the smell of old paper spill into the hall.", "stop", 0, 0), nil
}

func main() {
	ctx := context.Background()
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}

	// 1. Engine: in-memory SQLite + your generator. Zero infra, no API key.
	db, err := sql.Open("sqlite", "file:quickstart?mode=memory&cache=shared")
	must(err)
	db.SetMaxOpenConns(1)
	must(schema.NewLoader(schema.DialectSQLite).Apply(ctx, db))

	e, err := noewright.New(noewright.Config{
		DB: db, Dialect: noewright.DialectSQLite,
		Generators: map[string]noewright.Generator{"model": storyGen{}},
	})
	must(err)

	// 2. Prompts. User templates render with the turn's Inputs ({{.Inputs.x}}).
	authorSys := prompt(ctx, e, "author-sys", noewright.PromptKindSystem, "You are a narrator. Write one vivid sentence.")
	authorUser := prompt(ctx, e, "author-user", noewright.PromptKindUserTemplate, "The player does: {{.Inputs.action}}")
	criticSys := prompt(ctx, e, "critic-sys", noewright.PromptKindSystem, "You are a critic. Review the scene.")
	criticUser := prompt(ctx, e, "critic-user", noewright.PromptKindUserTemplate, "Review this scene: {{.Inputs.Lead}}")

	// 3. Agents: an author (the lead) and a critic (a follower).
	must(e.Agents().Create(ctx, &noewright.Agent{Slug: "author", Version: 1, Modal: noewright.ModalityText,
		GeneratorSlug: "model", SystemPromptID: authorSys, UserTemplateID: authorUser}))
	must(e.Agents().Create(ctx, &noewright.Agent{Slug: "critic", Version: 1, Modal: noewright.ModalityText,
		GeneratorSlug: "model", SystemPromptID: criticSys, UserTemplateID: criticUser}))

	// 4. A session (one playthrough), then one multi-agent turn.
	sess := &noewright.Session{PlatformID: "demo", State: noewright.State{Modality: noewright.ModalityText}}
	must(e.Sessions().Create(ctx, sess))

	turn, err := e.RunTurn(ctx, sess, noewright.TurnRequest{
		Flow: noewright.Flow{
			Slug:      "story",
			Lead:      noewright.FlowAgent{AgentSlug: "author", OutputKey: "Lead"}, // exposed to followers as {{.Inputs.Lead}}
			Followers: []noewright.FlowAgent{{AgentSlug: "critic"}},                // runs in parallel after the lead
		},
		Inputs: map[string]any{"action": "open the creaking door"},
	})
	must(err)

	// 5. Read each agent's output.
	fmt.Println("ACTION   :", "open the creaking door")
	fmt.Println("AUTHOR   :", noewright.ResultText(turn.Lead.Result))
	fmt.Println("CRITIC   :", noewright.ResultText(turn.Followers["critic"].Result))
}

func prompt(ctx context.Context, e *noewright.Engine, slug string, kind noewright.PromptKind, body string) uuid.UUID {
	p := &noewright.Prompt{Slug: slug, Version: 1, Kind: kind, Body: body}
	if err := e.Prompts().Create(ctx, p); err != nil {
		panic(err)
	}
	return p.ID
}
