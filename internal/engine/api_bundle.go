package engine

// api_bundle.go — bundles: one file that carries a configuration (prompts,
// response formats, agents, flows, graphs) out of one database and into
// another, with a digest that catches edits and damage.
//
// Records refer to each other by slug and version ("triage@2"), never by
// database id. A bundle never carries code or secrets: generators are named by
// slug and mapped to registered generators on import, Go functions are listed
// by name and must be registered by the importing app, and per-request
// overrides (API keys) are refused on save, so they are never exported.

import (
	"context"
	"errors"
)

// BundleFormat is the format version this engine writes and reads.
const BundleFormat = "bundle/v1"

// ErrInvalidBundle is returned for a bundle that cannot be read or imported as
// written: an unknown format, a digest that does not match, a reference to a
// record the bundle does not contain, or a record that fails its own checks.
var ErrInvalidBundle = errors.New("noewright: invalid bundle")

// ErrBundleConflict is returned by Import when the target already holds a
// record under the same slug and version with different content, and the
// import's OnConflict is ConflictFail. Nothing is written.
var ErrBundleConflict = errors.New("noewright: bundle conflicts with existing records")

// Bundle is a portable configuration. Marshal writes it as YAML; ParseBundle
// reads YAML or JSON.
type Bundle struct {
	Format    string `json:"noewright" yaml:"noewright"`
	Name      string `json:"name,omitempty" yaml:"name,omitempty"`
	CreatedBy string `json:"created_by,omitempty" yaml:"created_by,omitempty"`
	CreatedAt string `json:"created_at,omitempty" yaml:"created_at,omitempty"`

	Requires        BundleRequires         `json:"requires" yaml:"requires"`
	ResponseFormats []BundleResponseFormat `json:"response_formats,omitempty" yaml:"response_formats,omitempty"`
	Prompts         []BundlePrompt         `json:"prompts,omitempty" yaml:"prompts,omitempty"`
	Agents          []BundleAgent          `json:"agents,omitempty" yaml:"agents,omitempty"`
	Flows           []BundleFlow           `json:"flows,omitempty" yaml:"flows,omitempty"`
	Graphs          []BundleGraph          `json:"graphs,omitempty" yaml:"graphs,omitempty"`

	// Digest is "sha256:<hex>" over the canonical JSON of every field above.
	// ParseBundle refuses a bundle whose digest does not match; an empty digest
	// (a hand-written file) is accepted.
	Digest string `json:"digest,omitempty" yaml:"digest,omitempty"`
}

// BundleRequires is what the importing system must provide.
type BundleRequires struct {
	// Generators the agents use, by the slug they were registered under where
	// the bundle was made. ImportOptions.Generators maps them to local slugs.
	Generators []BundleGenerator `json:"generators,omitempty" yaml:"generators,omitempty"`
	// Funcs are the Go functions graph func nodes call. The importing app must
	// register them with Engine.RegisterFunc.
	Funcs []string `json:"funcs,omitempty" yaml:"funcs,omitempty"`
	// Evaluator is true when a graph has decide nodes.
	Evaluator bool `json:"evaluator,omitempty" yaml:"evaluator,omitempty"`
}

// BundleGenerator names a generator a bundle's agents use.
type BundleGenerator struct {
	Slug     string   `json:"slug" yaml:"slug"`
	Modality Modality `json:"modality,omitempty" yaml:"modality,omitempty"`
}

// BundlePrompt is a prompt record.
type BundlePrompt struct {
	Slug      string         `json:"slug" yaml:"slug"`
	Version   int            `json:"version" yaml:"version"`
	Kind      PromptKind     `json:"kind" yaml:"kind"`
	Category  string         `json:"category,omitempty" yaml:"category,omitempty"`
	Body      string         `json:"body" yaml:"body"`
	Variables []string       `json:"variables,omitempty" yaml:"variables,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Notes     string         `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// BundleResponseFormat is a saved response-format record.
type BundleResponseFormat struct {
	Slug     string         `json:"slug" yaml:"slug"`
	Version  int            `json:"version" yaml:"version"`
	Category string         `json:"category,omitempty" yaml:"category,omitempty"`
	Schema   map[string]any `json:"schema,omitempty" yaml:"schema,omitempty"`
	Strict   bool           `json:"strict,omitempty" yaml:"strict,omitempty"`
}

// BundleAgent is an agent record. Prompt, format and fallback references are
// "slug@version".
type BundleAgent struct {
	Slug      string   `json:"slug" yaml:"slug"`
	Version   int      `json:"version" yaml:"version"`
	Category  string   `json:"category,omitempty" yaml:"category,omitempty"`
	Modality  Modality `json:"modality" yaml:"modality"`
	Generator string   `json:"generator" yaml:"generator"`

	SystemPrompt   string `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	UserTemplate   string `json:"user_template,omitempty" yaml:"user_template,omitempty"`
	ResponseFormat string `json:"response_format,omitempty" yaml:"response_format,omitempty"`
	// InlineResponseFormat is an agent's own format, when it uses no saved one.
	InlineResponseFormat *ResponseFormat `json:"inline_response_format,omitempty" yaml:"inline_response_format,omitempty"`
	Params               GenerateParams  `json:"params,omitzero" yaml:"params,omitempty"`
	Fallback             string          `json:"fallback,omitempty" yaml:"fallback,omitempty"`
}

// BundleFlow is a flow record.
type BundleFlow struct {
	Slug     string            `json:"slug" yaml:"slug"`
	Version  int               `json:"version" yaml:"version"`
	Category string            `json:"category,omitempty" yaml:"category,omitempty"`
	Inputs   []string          `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	Agents   []BundleFlowAgent `json:"agents" yaml:"agents"`
}

// BundleFlowAgent is one agent slot in a flow; the first is the lead.
type BundleFlowAgent struct {
	Agent             string         `json:"agent" yaml:"agent"`
	Version           int            `json:"version,omitempty" yaml:"version,omitempty"`
	Output            string         `json:"output,omitempty" yaml:"output,omitempty"`
	Stream            bool           `json:"stream,omitempty" yaml:"stream,omitempty"`
	GeneratorOverride string         `json:"generator_override,omitempty" yaml:"generator_override,omitempty"`
	Params            map[string]any `json:"params,omitempty" yaml:"params,omitempty"`
	RetryMode         RetryMode      `json:"retry_mode,omitempty" yaml:"retry_mode,omitempty"`
	MaxRetries        int            `json:"max_retries,omitempty" yaml:"max_retries,omitempty"`
}

// BundleGraph is a graph record.
type BundleGraph struct {
	Slug     string `json:"slug" yaml:"slug"`
	Version  int    `json:"version" yaml:"version"`
	Category string `json:"category,omitempty" yaml:"category,omitempty"`
	Graph    Graph  `json:"graph" yaml:"graph"`
}

// BundleRoot names a record Export starts from. Kind is "graph", "flow" or
// "agent"; Version 0 is the latest active graph or flow, or the latest agent.
type BundleRoot struct {
	Kind    string
	Slug    string
	Version int
}

// ConflictPolicy says what Import does when the target holds a different
// record under the same slug and version.
type ConflictPolicy string

const (
	// ConflictFail stops the import; nothing is written. The default.
	ConflictFail ConflictPolicy = "fail"
	// ConflictBump imports the record as the next free version and updates the
	// bundle's references to it.
	ConflictBump ConflictPolicy = "bump"
)

// ImportOptions control an import.
type ImportOptions struct {
	// Owner is the scope the records are written to ("" = global).
	Owner string
	// Generators maps a bundle generator slug to a generator registered here.
	// A slug with no entry maps to itself.
	Generators map[string]string
	OnConflict ConflictPolicy
	// Activate makes imported flows and graphs the active version. Without it
	// they arrive as drafts and live sessions are unaffected.
	Activate bool
	// DryRun plans the import and reports it without writing anything.
	DryRun bool
	// SkipRuntimeChecks skips the checks that need the importing app's own
	// registrations (generators and Go functions). The CLI sets it; the app
	// should run Verify at startup instead.
	SkipRuntimeChecks bool
}

// ImportOutcome is what happened to one record.
type ImportOutcome string

const (
	ImportCreate   ImportOutcome = "create"   // not in the target: written
	ImportSkip     ImportOutcome = "skip"     // already there, identical
	ImportConflict ImportOutcome = "conflict" // already there, different
	ImportBump     ImportOutcome = "bump"     // different: written as NewVersion
)

// ImportItem is one record's line in an import report.
type ImportItem struct {
	Kind       string // "response_format", "prompt", "agent", "flow", "graph"
	Slug       string
	Version    int
	Outcome    ImportOutcome
	NewVersion int // for ImportBump
}

// ImportReport is what Import planned and, unless it was a dry run or failed,
// wrote.
type ImportReport struct {
	Items    []ImportItem
	Written  bool
	Warnings []string
}

// BundleRegistry exports and imports bundles.
type BundleRegistry interface {
	// Export starts from roots and follows every reference (sub-records of
	// graphs and flows, agents' prompts, formats and fallbacks), returning
	// exactly that set without the owner. Lists are sorted, so the same records
	// always export to the same bytes.
	Export(ctx context.Context, owner string, roots ...BundleRoot) (*Bundle, error)
	// Import plans every record (create, skip, conflict or bump) and writes
	// them in one transaction, in dependency order. A conflict under
	// ConflictFail returns ErrBundleConflict with the report and writes nothing.
	Import(ctx context.Context, b *Bundle, opts ImportOptions) (*ImportReport, error)
	// Verify lists what the importing system is missing: unregistered
	// generators and functions, an Evaluator a graph needs, and graphs that
	// would not run. Empty means ready.
	Verify(ctx context.Context, b *Bundle, opts ImportOptions) []string
}
