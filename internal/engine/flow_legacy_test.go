package engine

// flow_legacy_test.go — the pre-graph RunTurn, kept verbatim (renamed) so the
// differential test can prove the graph-backed RunTurn behaves identically.
// Delete once the graph executor has shipped in a release.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"

	"github.com/google/uuid"
)

func (e *Engine) runTurnLegacy(ctx context.Context, session *Session, req TurnRequest) (*Turn, error) {
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

	// ---- Lead ----
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
	leadStep, err := e.steps.run(ctx, session, StepRequest{
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
		Inputs:               agentInputs(inputs, req.Flow.Lead.Inputs),
		Params:               mergeParams(req.Params, req.Flow.Lead.Params),
		Overrides:            pickOverrides(req.Flow.Lead.Overrides, req.Overrides),
		GeneratorOverride:    req.Flow.Lead.GeneratorOverride,
		turnID:               turnID,
		turnRole:             "lead",
		bus:                  bus,
	})
	// ErrSessionNotPersisted means the lead step itself ran and committed — only
	// the session row is stale. The step is valid, so the turn proceeds and the
	// error is surfaced on Turn.Errors rather than aborting siblings that would
	// otherwise have succeeded.
	var leadNotPersisted error
	if errors.Is(err, ErrSessionNotPersisted) {
		leadNotPersisted, err = err, nil
	}
	if err != nil {
		return nil, fmt.Errorf("noewright: turn %q lead %q: %w", req.Flow.Slug, req.Flow.Lead.AgentSlug, err)
	}
	inputs[leadKey] = ResultText(leadStep.Result)
	bus.Publish(leadSlug, "lead.done", ResultText(leadStep.Result))

	turn := &Turn{
		ID:        turnID,
		Lead:      leadStep,
		Followers: map[string]*Step{},
		Errors:    map[string]error{},
		Steps:     []*Step{leadStep},
	}
	// The lead ran, but its session row write did not land. The step stands; the
	// caller is told under the "lead" key so it can reconcile before trusting
	// Session.State.
	if leadNotPersisted != nil {
		turn.Errors["lead"] = leadNotPersisted
		e.log.Error("turn lead session row not persisted", "turn_id", turnID, "agent", leadSlug, "err", leadNotPersisted)
	}
	// Fire the lead callback synchronously, before any follower goroutine starts,
	// so an embedder can build the follower inputs from the lead's output without
	// racing the fan-out.
	if req.OnStep != nil {
		req.OnStep("lead", leadStep, nil)
	}

	// ---- Followers (dependency-layered) ----
	// Followers run concurrently within a layer; a follower that consumes another
	// follower's output runs in a later layer so its producer has finished. A
	// flow with no cross-follower dependencies is a single layer — identical to
	// the historical all-concurrent fan-out.
	//
	// Within a layer the shared inputs map is only READ (each follower clones it
	// via agentInputs), and it is written ONLY at a layer barrier below, when no
	// follower goroutine is running — so the map write never races a read.
	followers := req.Flow.Followers
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

	var mu sync.Mutex
	runFollower := func(f FlowAgent) {
		role := "follower:" + f.AgentSlug
		fired := false
		// A follower panic (in the generator, a hook, or an OnStep callback)
		// must degrade only this follower, not crash the whole process. Record
		// it as a follower error and, if the normal path had not already fired
		// OnStep, surface it there too.
		defer func() {
			if r := recover(); r != nil {
				perr := fmt.Errorf("noewright: follower %q panicked: %v", f.AgentSlug, r)
				mu.Lock()
				if _, ok := turn.Followers[f.AgentSlug]; !ok {
					turn.Errors[f.AgentSlug] = perr
				}
				mu.Unlock()
				e.log.Error("turn follower panicked", "turn_id", turnID, "agent", f.AgentSlug, "err", perr)
				if req.OnStep != nil && !fired {
					req.OnStep(role, nil, perr)
				}
			}
		}()
		// This follower's own copy of the inputs, built once and shared by the
		// gate and the step so the gate decides on exactly what the agent would
		// have received.
		in := agentInputs(inputs, f.Inputs)

		// Gate: an opt-in check that can skip this follower entirely. A skip is
		// not a failure — no step is opened and nothing is recorded in
		// turn.Errors — so a caller distinguishes "we chose not to run this"
		// from "this ran and broke" by looking at Skipped rather than Errors.
		if f.When != nil {
			run, gerr := f.When(ctx, FollowerGate{
				Agent:   f,
				Session: session,
				Lead:    leadStep,
				Inputs:  in,
				TurnID:  turnID,
			})
			if gerr != nil {
				// A gate that cannot decide must not silently pick one way.
				// Report it and skip, so the failure is visible on the turn
				// while the rest of the fan-out still completes.
				gerr = fmt.Errorf("noewright: follower %q gate: %w", f.AgentSlug, gerr)
				mu.Lock()
				turn.Errors[f.AgentSlug] = gerr
				turn.Skipped = append(turn.Skipped, f.AgentSlug)
				mu.Unlock()
				e.log.Error("turn follower gate failed", "turn_id", turnID, "agent", f.AgentSlug, "err", gerr)
				if req.OnStep != nil {
					fired = true
					req.OnStep(role, nil, gerr)
				}
				return
			}
			if !run {
				mu.Lock()
				turn.Skipped = append(turn.Skipped, f.AgentSlug)
				mu.Unlock()
				return
			}
		}

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
		// the session row is stale. Treat the step as a success (publish it,
		// record it) while still reporting the error, so a stale row does not
		// silently drop a follower's output from the turn.
		notPersisted := errors.Is(ferr, ErrSessionNotPersisted)
		if ferr == nil || notPersisted {
			// Publish the follower's output so concurrent siblings can react.
			bus.Publish(f.AgentSlug, f.AgentSlug, ResultText(step.Result))
		}
		// Record the outcome under the mutex (map/slice writes only)...
		mu.Lock()
		if ferr != nil {
			turn.Errors[f.AgentSlug] = ferr
			e.log.Error("turn follower failed", "turn_id", turnID, "agent", f.AgentSlug, "err", ferr)
		}
		if ferr == nil || notPersisted {
			turn.Followers[f.AgentSlug] = step
			turn.Steps = append(turn.Steps, step)
		}
		mu.Unlock()
		// ...then fire OnStep OUTSIDE the mutex so a slow callback on one
		// follower cannot serialize the others.
		if req.OnStep != nil {
			fired = true
			switch {
			case notPersisted:
				// Valid step, stale session row — hand back both.
				req.OnStep(role, step, ferr)
			case ferr != nil:
				req.OnStep(role, nil, ferr)
			default:
				req.OnStep(role, step, nil)
			}
		}
	}

	for _, layer := range layers {
		var wg sync.WaitGroup
		for _, idx := range layer {
			f := followers[idx]
			wg.Add(1)
			go func(f FlowAgent) {
				defer wg.Done()
				runFollower(f)
			}(f)
		}
		wg.Wait()
		// Barrier: publish this layer's outputs into the shared inputs so the
		// next layer's templates can read them. Single-threaded here (all layer
		// goroutines have returned), so the map write cannot race a follower.
		// A follower that failed is absent from turn.Followers, so its output is
		// simply not injected — a consumer that declared it then fails at render
		// with ErrMissingVariables, attributing the missing dependency.
		for _, idx := range layer {
			f := followers[idx]
			if f.OutputKey == "" {
				continue
			}
			if step, ok := turn.Followers[f.AgentSlug]; ok {
				inputs[f.OutputKey] = ResultText(step.Result)
			}
		}
	}

	// The turn's dominant modality is the lead's output.
	session.State.Modality = leadStep.Result.Modality()

	turn.Messages = bus.Messages()
	e.emit(ctx, SessionEvent{Type: EventTurnDone, SessionID: session.ID, StepIndex: leadStep.Index, TurnID: turnID})
	return turn, nil
}
