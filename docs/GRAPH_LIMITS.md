# Graph engine: known limits

Decisions and limits accepted while building the graph engine
(branch `feature/graph-engine`), to revisit once the phases are done.
Each entry says what happens today and where it lives.

| # | Limit | What happens today | Where |
|---|---|---|---|
| 1 | **Saved graphs refuse per-agent `Overrides`** | `Graphs().Create` returns `ErrInvalidGraph` when any node sets `Agent.Overrides`, because overrides carry API keys and must never be stored. Pass them per run on `GraphRequest.Overrides`. In-code graphs passed to `RunGraph` may still set them. | `graph_store.go` `Create` |
| 2 | **One wait at a time** | If parallel branches reach two different wait nodes in one run, `RunGraph` returns `ErrInvalidGraph` and pauses at the first. Checked at run time, not when a graph is saved. | `graph_run.go` `RunGraph` |
| 3 | **Pausing writes twice** | The cursor goes into the last step's checkpoint, then the session row: two writes, not one transaction. If the second fails, `RunGraph` returns `ErrSessionNotPersisted`; a fork from that step still resumes at the wait. | `graph_run.go`, `store_step.go` `sqlUpdateSnapshot` |
| 4 | **A pause with no step has no checkpoint** | A run that pauses before recording any step (only functions or decisions ran) stores the cursor on the session row only, so there is no step to fork from at that point. The session itself resumes fine. | `graph_run.go` |
| 5 | **`Agent.When` is not used in graphs** | A graph node that sets the Go gate `FlowAgent.When` is refused; graphs route with edge conditions instead. `When` is never encoded. | `graph_run.go` `compileGraph`, `flow.go` |
| 6 | **Hook writes to vars are last-writer-wins** | `Validate` catches parallel `Save` mappings to a var with no reducer, but writes made by hooks are Go code it cannot see; they keep the existing rule (last writer wins, clash logged). | `api_session.go` `mergeState` |
| 7 | **Expression errors read as false** | A `when` that fails at run time does not fire and is recorded on `GraphRun.Errors` under `<node>.edge<k>`; a failing `Save` is skipped and recorded under `<node>.save.<var>`. | `graph_run.go` `graphNodeChoose` |
| 8 | **`decide` over structured output** | `Over` names an input; a non-string value is sent to the Evaluator as `fmt.Sprint` text, not JSON. | `graph_run.go` `graphNodeWork` |
| 9 | **Run cost leaves out Evaluator calls** | `GraphRun.Cost` totals the cost rows of the run's steps. `decide` calls are not in the cost ledger. | `graph_run.go` `runCost` |
| 10 | **Plan cost is a 30-day mean** | `Graphs().Plan` estimates each agent node from `Cost().AgentStats` over 30 days; no history reads as 0. | `graph_store.go` `Plan` |
| 11 | **No `MaxNodes` per call** | Each node runs at most once per call (loops only continue on a later call, through a wait), so there is no per-call node cap; `MaxVisits` caps a node across calls. | `graph_exec.go` |
| 12 | **Legacy `RunTurn` kept for the differential test** | `flow_legacy_test.go` holds the pre-graph `RunTurn` so `TestRunTurnMatchesLegacy` can compare the two. Delete both once the graph executor has shipped in a release. | `internal/engine/flow_legacy_test.go` |
| 13 | **`Turn.Messages` fix changes timing** | `Bus.Messages` now waits for the dispatcher to log earlier publishes, so it can block briefly; before, it could miss the newest messages. | `bus.go` |
| 14 | **Activation is not in the import transaction** | With `Activate`, records are written in one transaction, then each flow and graph is made active with `SetActive` after the commit. If an activation fails, the records exist but stay drafts. | `bundle.go` `Import` |
| 15 | **Content hashes are computed, not stored** | The design planned a `content_hash` column. Import instead renders the existing record in bundle form and hashes it each time. Correct, but slower for very large imports. | `bundle.go` `recordHash` |
| 16 | **`generator_override` is not mapped** | `--map` / `ImportOptions.Generators` maps agents' generators. A flow entry's or graph node's `generator_override` names a generator by slug and is carried as written. | `bundle.go` |
| 17 | **Version 0 references follow the target** | A graph node or flow entry that names an agent with version 0 (latest) exports the source's latest agent, but on import still means "latest" in the target, which may be a different version. Pin versions in anything you export. | `bundle.go` `Export` |
| 18 | **Verify does not call the model** | `Bundles().Verify` checks generators are registered, not that their keys and models work (`VerifyGenerator` makes a paid call). | `bundle.go` `Verify` |
| 19 | **CLI imports skip app checks** | The CLI has no app functions or real generators, so it imports with `SkipRuntimeChecks` and prints what the app must register. A graph whose function is not registered is refused by `RunGraph` until it is. | `cmd/noewright/bundle.go` |
| 20 | **Offline bundle tests use the echo generator** | `noewright test bundle.yaml` maps every generator to echo, so it checks routing, waits and templates, not model output, and refuses bundles that need app functions or an Evaluator. Real checks run through `Bundles().Test` in the app. | `cmd/noewright/bundle.go` `testBundle` |
| 21 | **Evidence output changes are reported, not failed** | A re-run whose output hash differs from the bundle's evidence lists it on `BundleTestResult.Changed`; it does not fail the test. Evidence records the generator when the run chose one for every agent (`Model`), not each agent's own model otherwise. | `bundle_tests.go` |
| 22 | **Tests run in new sessions in the target database** | `Bundles().Test` creates a real session per test (tagged `bundle-test`), with real steps and cost rows. Nothing cleans them up. | `bundle_tests.go` `runTest` |
