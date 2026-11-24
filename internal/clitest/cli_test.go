// Package clitest contains integration tests for the noewright binary.
// Each test builds the CLI binary once (TestMain) and then exercises the
// migrate, seed, and test subcommands against a real Postgres database.
//
// Tests are skipped when NOEWRIGHT_DSN is not set.
package clitest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rhaqim/noewright/internal/dsnenv"
)

var cliBin string // path to the compiled noewright binary

// TestMain compiles the noewright binary before any test runs. It builds even
// without NOEWRIGHT_DSN: some commands (verify, schema) need no database, and
// the database-backed tests skip themselves through requireDSN.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "noewright-test-*")
	if err != nil {
		panic("tmpdir: " + err.Error())
	}
	defer os.RemoveAll(tmp)

	cliBin = filepath.Join(tmp, "noewright")

	// Find the module root (three levels up from internal/clitest).
	moduleRoot := filepath.Join("..", "..")
	cmd := exec.Command("go", "build", "-o", cliBin, "./cmd/noewright")
	cmd.Dir = moduleRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("build noewright: " + err.Error())
	}

	os.Exit(m.Run())
}

// run executes the CLI binary with the given args and returns stdout+stderr.
func run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(cliBin, args...)
	cmd.Env = append(os.Environ(), dsnenv.Var+"="+dsnenv.Get())
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ex, ok := err.(*exec.ExitError); ok {
			code = ex.ExitCode()
		} else {
			t.Fatalf("exec: %v", err)
		}
	}
	return string(out), code
}

// requireDSN skips the test if NOEWRIGHT_DSN is unset.
func requireDSN(t *testing.T) {
	t.Helper()
	if dsnenv.Get() == "" {
		t.Skip("NOEWRIGHT_DSN not set — skipping integration test")
	}
}

// -----------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------

func TestCLI_Migrate(t *testing.T) {
	requireDSN(t)

	out, code := run(t, "migrate")
	if code != 0 {
		t.Fatalf("migrate exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "Schema applied") {
		t.Errorf("expected 'Schema applied' in output, got:\n%s", out)
	}
}

func TestCLI_Migrate_Idempotent(t *testing.T) {
	requireDSN(t)

	// Running migrate twice should succeed both times.
	for i := range 2 {
		out, code := run(t, "migrate")
		if code != 0 {
			t.Fatalf("migrate run %d exited %d:\n%s", i+1, code, out)
		}
	}
}

func TestCLI_Seed(t *testing.T) {
	requireDSN(t)

	// migrate first so tables exist.
	run(t, "migrate")

	seedFile := writeTempFile(t, "seed-*.yaml", seedYAML)
	out, code := run(t, "seed", seedFile)
	if code != 0 {
		t.Fatalf("seed exited %d:\n%s", code, out)
	}
	// Output contains either "seeded" (first run) or "skipped" (already exists).
	if !strings.Contains(out, "seeded") && !strings.Contains(out, "skipped") {
		t.Errorf("expected 'seeded' or 'skipped' in output, got:\n%s", out)
	}
}

func TestCLI_Seed_Idempotent(t *testing.T) {
	requireDSN(t)
	run(t, "migrate")

	// Use a unique seed file per run to avoid clashing with TestCLI_Seed.
	seedFile := writeTempFile(t, "seed-idem-*.yaml", seedIdempotentYAML)

	// Running seed twice must not error — duplicate slug/version should be tolerated.
	for i := range 2 {
		out, code := run(t, "seed", seedFile)
		if code != 0 && i == 0 {
			t.Fatalf("seed first run exited %d:\n%s", code, out)
		}
	}
}

func TestCLI_Test_Pass(t *testing.T) {
	requireDSN(t)
	run(t, "migrate")
	run(t, "seed", writeTempFile(t, "seed-*.yaml", seedYAML))

	planFile := writeTempFile(t, "plan-pass-*.yaml", passingPlanYAML)
	out, code := run(t, "test", planFile)
	if code != 0 {
		t.Fatalf("test exited %d (expected pass):\n%s", code, out)
	}
	if !strings.Contains(out, "passed") && !strings.Contains(out, "PASS") {
		t.Errorf("expected pass indication in output:\n%s", out)
	}
}

func TestCLI_Test_Fail(t *testing.T) {
	requireDSN(t)
	run(t, "migrate")
	run(t, "seed", writeTempFile(t, "seed-*.yaml", seedYAML))

	// A plan that references a non-existent agent should cause the test command to fail.
	planFile := writeTempFile(t, "plan-fail-*.yaml", failingPlanYAML)
	out, code := run(t, "test", planFile)
	if code == 0 {
		t.Fatalf("expected non-zero exit for failing plan, got 0:\n%s", out)
	}
}

func TestCLI_Help(t *testing.T) {
	requireDSN(t)
	out, code := run(t, "--help")
	if code != 0 {
		t.Fatalf("--help exited %d", code)
	}
	for _, want := range []string{"migrate", "seed", "test"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in --help output:\n%s", want, out)
		}
	}
}

// -----------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------

const seedYAML = `
prompts:
  - slug: cli-test-sys
    version: 1
    kind: system
    category: cli-test
    body: "You are a CLI test assistant."
  - slug: cli-test-user
    version: 1
    kind: user_template
    category: cli-test
    body: "User: {{.Action.Payload.text}}"
agents:
  - slug: cli-test-agent
    version: 1
    category: cli-test
    modal: text
    generator_slug: echo
`

// seedIdempotentYAML uses different slugs so it doesn't conflict with seedYAML.
const seedIdempotentYAML = `
prompts:
  - slug: cli-test-idem-sys
    version: 1
    kind: system
    category: cli-test
    body: "Idempotency test system prompt."
agents:
  - slug: cli-test-idem-agent
    version: 1
    category: cli-test
    modal: text
    generator_slug: echo
`

// passingPlanYAML runs the echo generator via the cli-test-agent.
// The echo generator is wired into the CLI via cliGenerators() in cmd/noewright/main.go.
const passingPlanYAML = `
name: cli-smoke-pass
session:
  platform_id: cli-test-user
  tags: ["test:true"]
  steps:
    - agent_slug: cli-test-agent
      action_payload: "Hello from CLI test"
variants:
  providers: []
`

// failingPlanYAML references an agent that does not exist — the test command
// must exit non-zero when it cannot resolve the agent.
const failingPlanYAML = `
name: cli-smoke-fail
session:
  platform_id: cli-test-user
  tags: ["test:true"]
  steps:
    - agent_slug: nonexistent-agent-xyz
      action_payload: "Hello"
variants:
  providers: []
`

// writeTempFile creates a temp file with the given content and registers cleanup.
func writeTempFile(t *testing.T, pattern, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

// handBundle is a bundle written by hand: no digest, a generator the CLI does
// not register (the app would), and a graph with a function only the app has.
const handBundle = `noewright: bundle/v1
name: cli-test
requires:
  generators: [{slug: chat, modality: text}]
  funcs: [lookup-account]
prompts:
  - {slug: cli-ut, version: 1, kind: user_template, body: "T[{{.Inputs.action}}]"}
agents:
  - {slug: cli-triage, version: 1, modality: text, generator: chat, user_template: cli-ut@1}
graphs:
  - slug: cli-support
    version: 1
    graph:
      entry: intake
      nodes:
        - {id: intake, kind: agent, agent: {agent: cli-triage, version: 1, output: Ticket}}
        - {id: account, kind: func, func: lookup-account}
      edges:
        - {from: intake, to: [account]}
`

func TestCLI_BundleVerifyOffline(t *testing.T) {
	path := writeTempFile(t, "bundle-*.yaml", handBundle)
	out, code := run(t, "verify", path)
	if code != 0 {
		t.Fatalf("verify exited %d:\n%s", code, out)
	}
	for _, want := range []string{"OK: 1 prompts", `generator "chat"`, `function "lookup-account"`, "no digest"} {
		if !strings.Contains(out, want) {
			t.Errorf("verify output lacks %q:\n%s", want, out)
		}
	}
	broken := strings.Replace(handBundle, "user_template: cli-ut@1", "user_template: missing@1", 1)
	if out, code := run(t, "verify", writeTempFile(t, "bundle-bad-*.yaml", broken)); code == 0 || !strings.Contains(out, "missing@1") {
		t.Fatalf("verify of a dangling reference exited %d:\n%s", code, out)
	}
}

func TestCLI_BundleImportExport(t *testing.T) {
	requireDSN(t)
	run(t, "migrate")
	owner := fmt.Sprintf("cli-%d", time.Now().UnixNano())
	path := writeTempFile(t, "bundle-*.yaml", handBundle)

	out, code := run(t, "import", path, "--owner", owner, "--map", "chat=echo", "--dry-run")
	if code != 0 || !strings.Contains(out, "Dry run") || !strings.Contains(out, "create") {
		t.Fatalf("dry run exited %d:\n%s", code, out)
	}
	out, code = run(t, "import", path, "--owner", owner, "--map", "chat=echo", "--activate")
	if code != 0 || !strings.Contains(out, "Imported.") || !strings.Contains(out, `function "lookup-account" is not registered`) {
		t.Fatalf("import exited %d:\n%s", code, out)
	}
	out, code = run(t, "import", path, "--owner", owner, "--map", "chat=echo")
	if code != 0 || !strings.Contains(out, "Nothing to import") {
		t.Fatalf("second import exited %d:\n%s", code, out)
	}

	exported := filepath.Join(t.TempDir(), "out.yaml")
	if out, code := run(t, "export", "graph:cli-support", "--owner", owner, "-o", exported); code != 0 {
		t.Fatalf("export exited %d:\n%s", code, out)
	}
	data, err := os.ReadFile(exported)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "digest: sha256:") || !strings.Contains(string(data), "generator: echo") {
		t.Fatalf("exported bundle:\n%s", data)
	}
	if out, code := run(t, "verify", exported); code != 0 {
		t.Fatalf("verify of the exported bundle exited %d:\n%s", code, out)
	}
	changed := strings.Replace(handBundle, "T[{{.Inputs.action}}]", "CHANGED", 1)
	out, code = run(t, "import", writeTempFile(t, "bundle-chg-*.yaml", changed), "--owner", owner, "--map", "chat=echo")
	if code == 0 || !strings.Contains(out, "conflict") {
		t.Fatalf("conflicting import exited %d:\n%s", code, out)
	}
}

// echoBundle has no functions or decisions, so its tests run offline.
const echoBundle = `noewright: bundle/v1
requires:
  generators: [{slug: chat, modality: text}]
prompts:
  - {slug: e-ut, version: 1, kind: user_template, body: "HELLO {{.Inputs.action}}"}
  - {slug: e-reply-ut, version: 1, kind: user_template, body: "THANKS {{.Inputs.Name}}", variables: [Name]}
agents:
  - {slug: e-greet, version: 1, modality: text, generator: chat, user_template: e-ut@1}
  - {slug: e-reply, version: 1, modality: text, generator: chat, user_template: e-reply-ut@1}
graphs:
  - slug: e-ask
    version: 1
    graph:
      entry: greet
      nodes:
        - {id: greet, kind: agent, agent: {agent: e-greet, version: 1}}
        - {id: name, kind: wait, output: Name, wait: {expect: text}}
        - {id: reply, kind: agent, agent: {agent: e-reply, version: 1}}
      edges:
        - {from: greet, to: [name]}
        - {from: name, to: [reply]}
tests:
  - name: asks then thanks
    graph: e-ask@1
    steps:
      - {say: there, expect: {waiting: name, contains: {greet: "HELLO there"}}}
      - {say: Ada, expect: {waiting: done, contains: {reply: "THANKS Ada"}, no_errors: true}}
  - name: wrong on purpose
    graph: e-ask@1
    steps:
      - {say: there, expect: {waiting: done}}
`

func TestCLI_BundleTestOffline(t *testing.T) {
	out, code := run(t, "test", writeTempFile(t, "bundle-tests-*.yaml", echoBundle))
	if code == 0 {
		t.Fatalf("a failing test should exit non-zero:\n%s", out)
	}
	for _, want := range []string{"PASS  asks then thanks", "FAIL  wrong on purpose", `waits at "name"`, "1 of 2 tests passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if out, code := run(t, "test", writeTempFile(t, "bundle-func-*.yaml", handBundle+"tests: [{name: x, graph: cli-support@1, steps: [{}]}]\n")); code == 0 || !strings.Contains(out, "Bundles().Test") {
		t.Fatalf("a bundle needing app functions exited %d:\n%s", code, out)
	}
}
