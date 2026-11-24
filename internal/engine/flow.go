package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"

	"github.com/google/uuid"
)

// flow.go adds a multi-agent *turn* primitive on top of the single-agent step
// loop. A Flow declares a turn as one lead agent followed by any number of
// follower agents. Followers run in dependency layers — concurrently within a
// layer, and after any sibling whose output they consume — so the default of
// independent followers is a single concurrent fan-out. This is the composition
// unit interactive-fiction style platforms need: a streaming "author" produces
// prose, then a "logician", "sensory director", "titler", etc. analyse that
// prose in parallel (and, where wired, feed each other) — all as one turn.
//
// Every agent in a flow is an ordinary versioned noewright Agent, so the only thing
// that changes between products (or between text, image, video, spatial, and AR
// modalities) is the set of agents/prompts referenced by the Flow plus the
// hooks registered on the engine. The engine machinery — sessions, retries,
// cost, branching, persistence — is unchanged.

// FlowAgent is one agent participating in a turn.
type FlowAgent struct {
	// AgentSlug + AgentVersion address a registered agent (version 0 = latest).
	AgentSlug    string
	AgentVersion int
	// Stream, on the lead agent only, routes streaming chunks to TurnRequest.OnChunk.
	Stream bool
	// OutputKey is the key under which this agent's text output is exposed to
	// later agents in the same turn via {{.Inputs.<OutputKey>}}. For the lead it
	// defaults to "Lead"; for followers it is optional.
	OutputKey string
	// Overrides is opaque per-call data forwarded to the generator (e.g. a
	// per-user api key / model). Optional.
	Overrides map[string]any
	// GeneratorOverride routes this agent's call to a different registered
	// generator for this turn only. Optional.
	GeneratorOverride string
	// Params are this agent's tuning knobs (model + domain), merged over the
	// turn's Params. See StepRequest.Params.
	Params map[string]any
	// RetryMode and MaxRetries override the engine retry defaults for this
	// agent's step (e.g. a keep-best lead, a discard follower). Zero values
	// inherit the engine default.
	RetryMode  RetryMode
	MaxRetries int
	// SystemPrompt, when non-zero, is this agent's per-turn system prompt
	// override (typically PromptRef{Literal: ...} assembled by the embedder).
	// The zero value leaves the agent's registered system prompt in place.
	SystemPrompt PromptRef
	// Inputs are merged over the turn's shared Inputs for this agent only, so
	// each agent can receive its own user_prompt / values while still seeing the
	// lead's output. Nil leaves the shared Inputs untouched.
	Inputs map[string]any
	// When, on a FOLLOWER, decides whether this agent runs at all this turn. It
	// is consulted after the lead settles (and, for a layered follower, after
	// its producers), with the inputs this agent would have received. Returning
	// false skips the follower: no step is opened, no generator is called, and
	// the slug is recorded on Turn.Skipped.
	//
	// Nil — the default — always runs, which is the historical behaviour.
	//
	// This is the cost lever the evaluation subsystem pays for: one cheap
	// classification deciding whether a full generation happens. An error from
	// the gate is recorded on Turn.Errors and the follower is skipped, so a
	// broken gate degrades to a missing follower rather than a failed turn.
	//
	// EXPERIMENTAL: this field is part of the opt-in evaluator work and may
	// change shape. It is inert unless set.
	When func(ctx context.Context, g FollowerGate) (bool, error)
}

// FollowerGate is what a FlowAgent.When gate is given to decide on. It carries
// the turn's state at the moment the follower would start.
//
// EXPERIMENTAL.
type FollowerGate struct {
	// Agent is the follower being gated.
	Agent FlowAgent
	// Session is the session the turn runs against.
	Session *Session
	// Lead is the lead agent's settled step. Its output is the usual thing to
	// gate on — read it with ResultText(g.Lead.Result).
	Lead *Step
	// Inputs are exactly the inputs this follower would receive, including the
	// lead's output under its OutputKey and any earlier layer's outputs. It is
	// this follower's own copy; mutating it affects nothing.
	Inputs map[string]any
	// TurnID identifies the turn, for correlating gate decisions with steps.
	TurnID uuid.UUID
}

// Flow declares a turn as a lead agent plus parallel followers.
type Flow struct {
	Slug      string
	Lead      FlowAgent
	Followers []FlowAgent
}

// TurnRequest is the input to RunTurn.
type TurnRequest struct {
	Flow Flow
	// Owner scopes registry resolution for every agent in the turn — the lead
	// and all followers — exactly as StepRequest.Owner does for a single step.
	// "" is the global scope. RunTurnBySlug sets this from the flow's own owner
	// so a persisted flow always resolves its agents within its own tenant.
	Owner string
	// Action is the user input that drives the turn (nil for an opening).
	Action *Action
	// OnChunk receives streaming fragments from the lead agent (if Lead.Stream).
	OnChunk func(Chunk)
	// Inputs seeds values available to every agent in the turn via
	// {{.Inputs.<key>}}. The lead's output is added under its OutputKey before
	// followers run.
	Inputs map[string]any
	// Overrides is opaque per-turn data forwarded to every agent's generator
	// unless an individual FlowAgent specifies its own Overrides.
	Overrides map[string]any
	// Params are turn-wide tuning knobs (model + domain, e.g. temperature,
	// tension) merged under each FlowAgent's own Params.
	Params map[string]any
	// OnStreamEnd fires when the lead's streamed draft is complete, before the
	// lead's post-hooks run, carrying the attempt index. It lets the caller close
	// its chunk sink the moment attempt 0 finishes even if a silent keep-best
	// retry follows. See StepRequest.OnStreamEnd.
	OnStreamEnd func(attempt int)
	// OnStep fires as each agent's step settles: once for the lead (role "lead",
	// err nil) guaranteed before any follower goroutine starts, then once per
	// follower (role "follower:<slug>") on success (step set, err nil) or failure
	// (step nil, err set), from the follower's own goroutine and outside the
	// turn's mutex. The callback MUST NOT block — a slow OnStep would stall the
	// turn. It lets an embedder build follower inputs from the lead and react to
	// each follower in completion order.
	OnStep func(role string, step *Step, err error)
	// LeadResult, when set, makes this a human-led turn: the lead step records
	// this result (written by LeadAuthor, e.g. a human game master) instead of
	// calling the lead agent's generator, then the followers run as usual and
	// consume it through the lead's OutputKey. It must be a ready result. The
	// recorded lead has no cost row and does not stream; its text reaches the
	// bus "lead.done" topic as any lead's does.
	LeadResult Result
	// LeadAuthor is stored on the recorded lead's Step.Author. Required with
	// LeadResult.
	LeadAuthor string
}

// Turn is the result of RunTurn: the lead step plus follower steps, all sharing
// a single turn ID (recorded on each Step's Diagnostics as "turn_id").
type Turn struct {
	ID        uuid.UUID
	Lead      *Step
	Followers map[string]*Step // keyed by follower AgentSlug (successes only)
	Errors    map[string]error // keyed by follower AgentSlug (failures only)
	Steps     []*Step          // lead first, then followers (completion order)
	Messages  []Message        // everything published on the turn's Bus
	// Skipped names the followers a FlowAgent.When gate declined to run, in
	// completion order. They are neither successes nor failures: no step
	// exists, and none was attempted. Empty unless a flow uses gates.
	//
	// EXPERIMENTAL, alongside FlowAgent.When.
	Skipped []string
}

// RunTurn executes a Flow against a session: it runs the lead agent (optionally
// streaming), injects the lead's text output into the shared Inputs, then runs
// the follower agents. Every resulting step is persisted and tagged with the
// same turn ID. A follower failure does not abort the turn — its error is
// recorded on the Turn but siblings still complete.
//
// Followers run in dependency LAYERS: a follower that consumes another
// follower's output (its user template declares the sibling's OutputKey in
// Prompt.Variables) runs in a later layer, after that producer, and receives
// its output under the producer's OutputKey. Followers within a layer run
// concurrently, and a flow with no cross-follower dependencies is a single
// layer — identical to the historical all-concurrent fan-out. Validate the
// wiring with Flows().Validate; a dependency cycle is rejected there, and if an
// in-code flow still contains one, the cyclic followers run last and fail on
// their missing input rather than deadlocking.
//
// A non-nil error means the lead step failed; it is the lead's RunStep error
// (see RunStep for the error contract) wrapped with the turn and agent slug.
// Follower failures are never returned here — inspect Turn.Errors (keyed by
// agent slug) for those.
//
// ErrSessionNotPersisted is the exception: it means the step itself ran and
// committed and only the session row is stale, so the turn is NOT aborted. The
// step appears in Turn.Steps (and Turn.Followers) as normal and the error is
// recorded in Turn.Errors under "lead" or the follower's agent slug. A turn can
// therefore return a nil error while carrying entries in Turn.Errors — check it
// before treating Session.State as authoritative.
func (e *Engine) RunTurn(ctx context.Context, session *Session, req TurnRequest) (*Turn, error) {
	turnID := uuid.New()

	inputs := map[string]any{}
	maps.Copy(inputs, req.Inputs)
	// Expose the turn's action text to every agent (lead and followers) under
	// "action" unless the caller already supplied it.
	if _, ok := inputs["action"]; !ok {
		if t := actionText(req.Action); t != "" {
			inputs["action"] = t
		}
	}

	// Per-turn cross-agent channel fabric, available to every agent's generator.
	bus := newBus()
	defer bus.close()

	leadKey := req.Flow.Lead.OutputKey
	if leadKey == "" {
		leadKey = "Lead"
	}
	leadSlug := req.Flow.Lead.AgentSlug
	// The lead's chunks are published on the "lead" topic as they stream, so a
	// follower can consume the lead's output live (and, via replay, even though
	// followers start after the lead).
	var onChunk func(Chunk)
	if req.Flow.Lead.Stream {
		onChunk = func(c Chunk) {
			bus.Publish(leadSlug, "lead", c.Content)
			if req.OnChunk != nil {
				req.OnChunk(c)
			}
		}
	}
	if req.LeadResult != nil {
		if req.LeadAuthor == "" {
			return nil, fmt.Errorf("noewright: TurnRequest.LeadResult needs a LeadAuthor")
		}
		onChunk = nil
	}

	// A turn is a graph with a fixed shape: the lead is the entry node, and the
	// followers form dependency layers after it. compileTurn builds that graph;
	// the shared executor runs it.
	g, leadStep := e.compileTurn(ctx, session, req, turnID, leadKey, onChunk, bus)
	res := runExecGraph(ctx, g, inputs)
	if res.abort != nil {
		return nil, fmt.Errorf("noewright: turn %q lead %q: %w", req.Flow.Slug, req.Flow.Lead.AgentSlug, res.abort)
	}

	turn := &Turn{
		ID:        turnID,
		Lead:      *leadStep,
		Followers: map[string]*Step{},
		Errors:    map[string]error{},
		Steps:     res.steps, // lead first, then followers in completion order
	}
	// Errors are replayed in the order they were recorded, keyed by follower
	// slug — or "lead" for a lead whose session row was not persisted — so a
	// later write wins exactly as it did when the turn wrote the map directly.
	for _, ce := range res.errs {
		turn.Errors[turnNodeKey(req.Flow, ce.node)] = ce.err
	}
	for k, step := range res.steps {
		if node := res.stepNodes[k]; node != 0 {
			turn.Followers[turnNodeKey(req.Flow, node)] = step
		}
	}
	for _, node := range res.skipped {
		turn.Skipped = append(turn.Skipped, turnNodeKey(req.Flow, node))
	}

	// The turn's dominant modality is the lead's output.
	session.State.Modality = turn.Lead.Result.Modality()

	turn.Messages = bus.Messages()
	e.emit(ctx, SessionEvent{Type: EventTurnDone, SessionID: session.ID, StepIndex: turn.Lead.Index, TurnID: turnID})
	return turn, nil
}

// turnNodeKey names a turn graph node the way Turn reports it: "lead" for the
// entry node, the follower's agent slug otherwise.
func turnNodeKey(f Flow, node int) string {
	if node == 0 {
		return "lead"
	}
	return f.Followers[node-1].AgentSlug
}

// compileTurn builds the executable graph for a turn. Node 0 is the lead,
// required and run on the caller's goroutine; node i (i ≥ 1) is follower i-1.
// Followers keep RunTurn's long-standing contract:
//
//   - They run in dependency layers (topoLayers over their templates'
//     declared variables). Every node in a layer has an edge from every node in
//     the layer before it (from the lead, for the first layer), so a layer
//     starts only when the whole previous layer has settled — the barrier the
//     turn has always had, which OnStep order and When gates rely on.
//   - A follower that fails, is gated out or panics still fires its edges, so
//     later layers run and a consumer of its output fails on the missing input.
//   - A follower's panic is recovered and recorded as its error.
//
// The returned pointer is set to the lead's step once the lead has run; it is
// read only after that (by follower gates and by RunTurn).
func (e *Engine) compileTurn(ctx context.Context, session *Session, req TurnRequest, turnID uuid.UUID,
	leadKey string, onChunk func(Chunk), bus *busImpl) (*execGraph, **Step) {
	leadStep := new(*Step)
	leadSlug := req.Flow.Lead.AgentSlug

	lead := execNode{
		outputKey: leadKey,
		inputs:    req.Flow.Lead.Inputs,
		required:  true,
		run: func(ctx context.Context, in map[string]any) (*Step, error) {
			step, err := e.steps.run(ctx, session, StepRequest{
				external:             req.LeadResult,
				author:               req.LeadAuthor,
				AgentSlug:            req.Flow.Lead.AgentSlug,
				AgentVersion:         req.Flow.Lead.AgentVersion,
				Owner:                req.Owner,
				Action:               req.Action,
				OnChunk:              onChunk,
				OnStreamEnd:          req.OnStreamEnd,
				RetryMode:            req.Flow.Lead.RetryMode,
				MaxRetries:           req.Flow.Lead.MaxRetries,
				SystemPromptOverride: promptRefOf(req.Flow.Lead.SystemPrompt),
				Inputs:               in,
				Params:               mergeParams(req.Params, req.Flow.Lead.Params),
				Overrides:            pickOverrides(req.Flow.Lead.Overrides, req.Overrides),
				GeneratorOverride:    req.Flow.Lead.GeneratorOverride,
				turnID:               turnID,
				turnRole:             "lead",
				bus:                  bus,
			})
			if stepSucceeded(step, err) {
				*leadStep = step
				bus.Publish(leadSlug, "lead.done", ResultText(step.Result))
			}
			return step, err
		},
		// ErrSessionNotPersisted means the lead step itself ran and committed —
		// only the session row is stale. The turn proceeds; the error is
		// surfaced on Turn.Errors["lead"]. The lead callback fires before any
		// follower starts, so an embedder can build follower inputs from it.
		settled: func(step *Step, err error) {
			if !stepSucceeded(step, err) {
				return
			}
			if err != nil {
				e.log.Error("turn lead session row not persisted", "turn_id", turnID, "agent", leadSlug, "err", err)
			}
			if req.OnStep != nil {
				req.OnStep("lead", step, nil)
			}
		},
	}

	g := &execGraph{nodes: []execNode{lead}, entry: 0}
	followers := req.Flow.Followers
	for _, f := range followers {
		g.nodes = append(g.nodes, e.followerNode(session, req, turnID, f, leadStep, bus))
	}
	g.succ = make([][]int, len(g.nodes))

	// Dependency layers, exactly as the turn has always computed them.
	outputKeys := make([]string, len(followers))
	consumed := make([][]string, len(followers))
	for i, f := range followers {
		outputKeys[i] = f.OutputKey
		consumed[i] = e.userTemplateVars(ctx, req.Owner, f.AgentSlug, f.AgentVersion)
	}
	layers, cyclic := topoLayers(outputKeys, consumed)
	if len(cyclic) > 0 {
		// Validate rejects a cycle at publish time; an in-code flow can still
		// reach here. Run the cyclic followers last so each fails on its missing
		// sibling input (ErrMissingVariables) rather than deadlocking the turn.
		e.log.Error("turn: follower dependency cycle; running cyclic followers last",
			"turn_id", turnID, "count", len(cyclic))
		layers = append(layers, cyclic)
	}
	prev := []int{0}
	for _, layer := range layers {
		var cur []int
		for _, idx := range layer {
			cur = append(cur, idx+1)
		}
		for _, p := range prev {
			g.succ[p] = append(g.succ[p], cur...)
		}
		prev = cur
	}
	return g, leadStep
}

// followerNode builds the graph node for one follower of a turn.
func (e *Engine) followerNode(session *Session, req TurnRequest, turnID uuid.UUID, f FlowAgent, leadStep **Step, bus *busImpl) execNode {
	role := "follower:" + f.AgentSlug
	node := execNode{
		outputKey:     f.OutputKey,
		inputs:        f.Inputs,
		fireOnFail:    true,
		recoverPanics: true,
		// A follower panic (in the generator, a hook, or an OnStep callback)
		// must degrade only this follower, not crash the whole process.
		onPanic: func(r any) error {
			perr := fmt.Errorf("noewright: follower %q panicked: %v", f.AgentSlug, r)
			e.log.Error("turn follower panicked", "turn_id", turnID, "agent", f.AgentSlug, "err", perr)
			return perr
		},
		run: func(ctx context.Context, in map[string]any) (*Step, error) {
			step, ferr := e.steps.run(ctx, session, StepRequest{
				AgentSlug:    f.AgentSlug,
				AgentVersion: f.AgentVersion,
				Owner:        req.Owner,
				// The turn's Action is recorded once, on the lead step. Followers
				// analyse the lead's output and read the action via Inputs, so they
				// carry no Action (sharing the lead's would duplicate its ID).
				Action:               nil,
				RetryMode:            f.RetryMode,
				MaxRetries:           f.MaxRetries,
				SystemPromptOverride: promptRefOf(f.SystemPrompt),
				Inputs:               in,
				Params:               mergeParams(req.Params, f.Params),
				Overrides:            pickOverrides(f.Overrides, req.Overrides),
				GeneratorOverride:    f.GeneratorOverride,
				turnID:               turnID,
				turnRole:             role,
				bus:                  bus,
			})
			// ErrSessionNotPersisted leaves a valid, committed step behind — only
			// the session row is stale. Publish it so siblings can react; the
			// executor records it as a success while still reporting the error.
			if stepSucceeded(step, ferr) {
				bus.Publish(f.AgentSlug, f.AgentSlug, ResultText(step.Result))
			}
			if ferr != nil {
				e.log.Error("turn follower failed", "turn_id", turnID, "agent", f.AgentSlug, "err", ferr)
			}
			return step, ferr
		},
		// OnStep fires from the follower's own goroutine, outside any lock, so a
		// slow callback on one follower cannot serialize the others.
		settled: func(step *Step, err error) {
			if req.OnStep == nil {
				return
			}
			switch {
			case step != nil && errors.Is(err, ErrSessionNotPersisted):
				req.OnStep(role, step, err) // valid step, stale session row
			case err != nil:
				req.OnStep(role, nil, err)
			default:
				req.OnStep(role, step, nil)
			}
		},
	}
	// Gate: an opt-in check that can skip this follower entirely. A skip is not
	// a failure — no step is opened and nothing is recorded in Turn.Errors. A
	// gate that cannot decide is reported and the follower skipped, so the
	// failure is visible while the rest of the fan-out completes.
	if f.When != nil {
		node.gate = func(ctx context.Context, in map[string]any) (bool, error) {
			run, gerr := f.When(ctx, FollowerGate{
				Agent:   f,
				Session: session,
				Lead:    *leadStep,
				Inputs:  in,
				TurnID:  turnID,
			})
			if gerr != nil {
				gerr = fmt.Errorf("noewright: follower %q gate: %w", f.AgentSlug, gerr)
				e.log.Error("turn follower gate failed", "turn_id", turnID, "agent", f.AgentSlug, "err", gerr)
			}
			return run, gerr
		}
	}
	return node
}

// topoLayers partitions N followers into dependency layers by Kahn's algorithm.
// outputKeys[i] is follower i's output name (empty = it produces nothing);
// consumed[i] is the input names follower i consumes. An edge j→i exists when
// follower i consumes follower j's output key, meaning i must run after j.
//
// Layer 0 holds followers that depend on no sibling; each later layer holds
// followers whose sibling producers all appear in an earlier layer. Consumed
// names that match no follower output key (external inputs, the lead's output)
// create no edge — the lead has already run and externals are present from the
// start, so they never constrain follower order.
//
// Followers caught in a dependency cycle cannot be ordered and are returned in
// `cyclic`; the caller decides what to do with them (Validate rejects the flow,
// RunTurn runs them in a final layer so they fail on their missing input rather
// than deadlock). Layer contents are sorted by index for deterministic order.
func topoLayers(outputKeys []string, consumed [][]string) (layers [][]int, cyclic []int) {
	n := len(outputKeys)
	producer := map[string]int{} // output name -> producing follower index
	for i, k := range outputKeys {
		if k != "" {
			producer[k] = i
		}
	}
	dependents := make([][]int, n) // j -> followers that depend on j
	indeg := make([]int, n)
	for i := range consumed {
		seen := map[int]bool{}
		for _, name := range consumed[i] {
			j, ok := producer[name]
			if !ok || j == i || seen[j] {
				continue
			}
			seen[j] = true
			dependents[j] = append(dependents[j], i)
			indeg[i]++
		}
	}

	var cur []int
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			cur = append(cur, i)
		}
	}
	placed := 0
	for len(cur) > 0 {
		sort.Ints(cur)
		layers = append(layers, cur)
		var next []int
		for _, j := range cur {
			placed++
			for _, i := range dependents[j] {
				indeg[i]--
				if indeg[i] == 0 {
					next = append(next, i)
				}
			}
		}
		cur = next
	}
	if placed < n {
		for i := 0; i < n; i++ {
			if indeg[i] > 0 {
				cyclic = append(cyclic, i)
			}
		}
	}
	return layers, cyclic
}

// userTemplateVars best-effort resolves the input-variable names an agent's user
// template declares (its Prompt.Variables), used to build the follower
// dependency graph. It returns nil on any resolution failure — a missing agent
// or prompt means "unknown dependencies", so the follower lands in the first
// layer and surfaces its own error when it actually runs, preserving RunTurn's
// per-follower failure isolation.
func (e *Engine) userTemplateVars(ctx context.Context, owner, slug string, version int) []string {
	agent, err := e.agents.Get(ctx, owner, slug, version)
	if err != nil || agent.UserTemplateID == uuid.Nil {
		return nil
	}
	ut, err := e.prompts.GetByID(ctx, agent.UserTemplateID)
	if err != nil {
		return nil
	}
	return ut.Variables
}

// mergeParams overlays per-agent params on turn-wide defaults (agent wins).
func mergeParams(turnParams, agentParams map[string]any) map[string]any {
	if len(turnParams) == 0 && len(agentParams) == 0 {
		return nil
	}
	out := make(map[string]any, len(turnParams)+len(agentParams))
	maps.Copy(out, turnParams)
	maps.Copy(out, agentParams)
	return out
}

// ResultText extracts the primary text payload from a Result for prompt
// injection, returning "" for modalities without a natural text form.
//
// A StructuredResult renders as its "text" field when it has one (the
// established convention) and otherwise as the JSON encoding of Data. It used
// to return "" in that case, so a structured agent's output reached the next
// agent in a turn as an empty input.
func ResultText(r Result) string {
	switch v := r.(type) {
	case *TextResult:
		return v.Content
	case *StructuredResult:
		if s, ok := v.Data["text"].(string); ok {
			return s
		}
		if len(v.Data) == 0 {
			return ""
		}
		if b, err := json.Marshal(v.Data); err == nil {
			return string(b)
		}
	case *ImageResult:
		return v.Prompt
	case *VideoResult:
		return v.PreviewImage
	case *Model3DResult:
		return v.Prompt
	}
	return ""
}

// actionText best-effort extracts a human-readable string from an Action for
// prompt/input injection. It understands the common free-text payload shape.
func actionText(a *Action) string {
	if a == nil {
		return ""
	}
	switch p := a.Payload.(type) {
	case map[string]any:
		if s, ok := p["text"].(string); ok {
			return s
		}
	case FreeText:
		return p.Body
	case string:
		return p
	}
	return ""
}

func cloneInputs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

// agentInputs clones the shared turn inputs and merges the agent's own Inputs on
// top, so each agent can carry its own values (e.g. a distinct user_prompt)
// without disturbing siblings. cloneInputs always returns a non-nil map.
func agentInputs(shared, own map[string]any) map[string]any {
	m := cloneInputs(shared)
	maps.Copy(m, own)
	return m
}

// promptRefOf returns a pointer to ref when it carries an override, or nil when
// it is the zero value (so the agent's registered system prompt is used).
func promptRefOf(ref PromptRef) *PromptRef {
	if ref == (PromptRef{}) {
		return nil
	}
	return &ref
}

func pickOverrides(specific, fallback map[string]any) map[string]any {
	if specific != nil {
		return specific
	}
	return fallback
}
