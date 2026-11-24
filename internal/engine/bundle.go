package engine

// bundle.go — Export, Import and Verify for bundles, and their file form.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

//go:embed bundle.schema.json
var bundleSchema []byte

// BundleSchema returns the JSON Schema for bundle files, for editors and other
// tools that check a bundle as it is written.
func BundleSchema() []byte { return slices.Clone(bundleSchema) }

// maxBundleBytes bounds what ParseBundle reads.
const maxBundleBytes = 16 << 20

type bundleService struct{ e *Engine }

// Bundles returns the bundle registry.
func (e *Engine) Bundles() BundleRegistry { return e.bundles }

// ParseBundle reads a bundle from YAML or JSON, checks its format version, and
// checks its digest when it has one.
func ParseBundle(data []byte) (*Bundle, error) {
	if len(data) > maxBundleBytes {
		return nil, fmt.Errorf("%w: %d bytes is over the %d-byte limit", ErrInvalidBundle, len(data), maxBundleBytes)
	}
	var b Bundle
	if err := yaml.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
	}
	if b.Format != BundleFormat {
		return nil, fmt.Errorf("%w: format %q, this engine reads %q", ErrInvalidBundle, b.Format, BundleFormat)
	}
	if b.Digest != "" {
		want, err := b.ComputeDigest()
		if err != nil {
			return nil, err
		}
		if b.Digest != want {
			return nil, fmt.Errorf("%w: digest %s does not match the content (%s); the file was edited or damaged", ErrInvalidBundle, b.Digest, want)
		}
	}
	return &b, nil
}

// ComputeDigest returns the digest of the bundle's content: everything except
// the Digest field itself.
func (b *Bundle) ComputeDigest() (string, error) {
	c := *b
	c.Digest = ""
	// Round-trip through YAML first, so the digest is the same whether it is
	// computed on an exported bundle or on the same bundle read from its file
	// (YAML decodes numbers as int where JSON decoding gave float64, and so on).
	y, err := yaml.Marshal(&c)
	if err != nil {
		return "", fmt.Errorf("%w: encode: %v", ErrInvalidBundle, err)
	}
	var canon Bundle
	if err := yaml.Unmarshal(y, &canon); err != nil {
		return "", fmt.Errorf("%w: encode: %v", ErrInvalidBundle, err)
	}
	j, err := json.Marshal(&canon)
	if err != nil {
		return "", fmt.Errorf("%w: encode: %v", ErrInvalidBundle, err)
	}
	sum := sha256.Sum256(j)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Marshal sets the bundle's digest and returns it as YAML.
func (b *Bundle) Marshal() ([]byte, error) {
	if b.Format == "" {
		b.Format = BundleFormat
	}
	d, err := b.ComputeDigest()
	if err != nil {
		return nil, err
	}
	b.Digest = d
	return yaml.Marshal(b)
}

// ref renders a "slug@version" reference.
func ref(slug string, version int) string { return slug + "@" + strconv.Itoa(version) }

// parseRef splits a "slug@version" reference.
func parseRef(r string) (string, int, error) {
	i := strings.LastIndexByte(r, '@')
	if i <= 0 {
		return "", 0, fmt.Errorf("%w: reference %q is not slug@version", ErrInvalidBundle, r)
	}
	v, err := strconv.Atoi(r[i+1:])
	if err != nil || v < 1 {
		return "", 0, fmt.Errorf("%w: reference %q has no version", ErrInvalidBundle, r)
	}
	return r[:i], v, nil
}

// recordHash is the content hash Import compares records by.
func recordHash(v any) string {
	// Same round trip as the digest, so a record read from a file and the same
	// record read from the database hash alike.
	y, _ := yaml.Marshal(v)
	out := reflectNew(v)
	_ = yaml.Unmarshal(y, out)
	j, _ := json.Marshal(out)
	sum := sha256.Sum256(j)
	return hex.EncodeToString(sum[:])
}

// reflectNew returns a pointer to a new zero value of v's type, for the bundle
// record types recordHash is used with.
func reflectNew(v any) any {
	switch v.(type) {
	case BundlePrompt:
		return &BundlePrompt{}
	case BundleResponseFormat:
		return &BundleResponseFormat{}
	case BundleAgent:
		return &BundleAgent{}
	case BundleFlow:
		return &BundleFlow{}
	case BundleGraph:
		return &BundleGraph{}
	}
	return new(map[string]any)
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

type exporter struct {
	e     *Engine
	owner string
	b     *Bundle
	seen  map[string]bool
	gens  map[string]Modality
	funcs map[string]bool
}

func (s *bundleService) Export(ctx context.Context, owner string, roots ...BundleRoot) (*Bundle, error) {
	x := &exporter{e: s.e, owner: owner, b: &Bundle{Format: BundleFormat}, seen: map[string]bool{},
		gens: map[string]Modality{}, funcs: map[string]bool{}}
	for _, r := range roots {
		var err error
		switch r.Kind {
		case "graph":
			err = x.graph(ctx, r.Slug, r.Version)
		case "flow":
			err = x.flow(ctx, r.Slug, r.Version)
		case "agent":
			err = x.agentBySlug(ctx, r.Slug, r.Version)
		default:
			err = fmt.Errorf("noewright: export: unknown root kind %q (graph, flow or agent)", r.Kind)
		}
		if err != nil {
			return nil, err
		}
	}
	b := x.b
	for slug, m := range x.gens {
		b.Requires.Generators = append(b.Requires.Generators, BundleGenerator{Slug: slug, Modality: m})
	}
	sort.Slice(b.Requires.Generators, func(i, j int) bool { return b.Requires.Generators[i].Slug < b.Requires.Generators[j].Slug })
	b.Requires.Funcs = slices.Sorted(maps.Keys(x.funcs))
	sort.Slice(b.ResponseFormats, func(i, j int) bool {
		return ref(b.ResponseFormats[i].Slug, b.ResponseFormats[i].Version) < ref(b.ResponseFormats[j].Slug, b.ResponseFormats[j].Version)
	})
	sort.Slice(b.Prompts, func(i, j int) bool {
		a, c := b.Prompts[i], b.Prompts[j]
		if a.Slug != c.Slug {
			return a.Slug < c.Slug
		}
		if a.Version != c.Version {
			return a.Version < c.Version
		}
		return a.Kind < c.Kind
	})
	sort.Slice(b.Agents, func(i, j int) bool {
		return b.Agents[i].Slug < b.Agents[j].Slug || (b.Agents[i].Slug == b.Agents[j].Slug && b.Agents[i].Version < b.Agents[j].Version)
	})
	sort.Slice(b.Flows, func(i, j int) bool {
		return b.Flows[i].Slug < b.Flows[j].Slug || (b.Flows[i].Slug == b.Flows[j].Slug && b.Flows[i].Version < b.Flows[j].Version)
	})
	sort.Slice(b.Graphs, func(i, j int) bool {
		return b.Graphs[i].Slug < b.Graphs[j].Slug || (b.Graphs[i].Slug == b.Graphs[j].Slug && b.Graphs[i].Version < b.Graphs[j].Version)
	})
	return b, nil
}

func (x *exporter) once(key string) bool {
	if x.seen[key] {
		return false
	}
	x.seen[key] = true
	return true
}

func (x *exporter) graph(ctx context.Context, slug string, version int) error {
	rec, err := x.e.graphs.Get(ctx, x.owner, slug, version)
	if err != nil {
		return fmt.Errorf("noewright: export graph %q: %w", slug, err)
	}
	if !x.once("graph:" + ref(rec.Slug, rec.Version)) {
		return nil
	}
	x.b.Graphs = append(x.b.Graphs, graphToBundle(rec))
	for _, n := range rec.Graph.Nodes {
		switch n.Kind {
		case NodeAgent:
			if err := x.agentBySlug(ctx, n.Agent.AgentSlug, n.Agent.AgentVersion); err != nil {
				return err
			}
		case NodeFunc:
			x.funcs[n.Func] = true
			if n.Record != "" {
				if err := x.agentBySlug(ctx, n.Record, 0); err != nil {
					return err
				}
			}
		case NodeDecide:
			x.b.Requires.Evaluator = true
		case NodeWait:
			if n.Wait != nil && n.Wait.Record != "" {
				if err := x.agentBySlug(ctx, n.Wait.Record, 0); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func graphToBundle(rec *GraphRecord) BundleGraph {
	g := rec.Graph
	g.Slug = "" // the record's slug is the graph's slug
	return BundleGraph{Slug: rec.Slug, Version: rec.Version, Category: rec.Category, Graph: g}
}

func (x *exporter) flow(ctx context.Context, slug string, version int) error {
	rec, err := x.e.flows.Get(ctx, x.owner, slug, version)
	if err != nil {
		return fmt.Errorf("noewright: export flow %q: %w", slug, err)
	}
	if !x.once("flow:" + ref(rec.Slug, rec.Version)) {
		return nil
	}
	x.b.Flows = append(x.b.Flows, flowToBundle(rec))
	for _, a := range rec.Agents {
		if err := x.agentBySlug(ctx, a.AgentSlug, a.AgentVersion); err != nil {
			return err
		}
	}
	return nil
}

func flowToBundle(rec *FlowRecord) BundleFlow {
	bf := BundleFlow{Slug: rec.Slug, Version: rec.Version, Category: rec.Category, Inputs: nilIfEmpty(rec.Inputs)}
	for _, a := range rec.Agents {
		bf.Agents = append(bf.Agents, BundleFlowAgent{Agent: a.AgentSlug, Version: a.AgentVersion, Output: a.OutputKey,
			Stream: a.Stream, GeneratorOverride: a.GeneratorOverride, Params: nilIfEmptyMap(a.Params),
			RetryMode: a.RetryMode, MaxRetries: a.MaxRetries})
	}
	return bf
}

func (x *exporter) agentBySlug(ctx context.Context, slug string, version int) error {
	a, err := x.e.agents.Get(ctx, x.owner, slug, version)
	if err != nil {
		return fmt.Errorf("noewright: export agent %q: %w", slug, err)
	}
	return x.agent(ctx, a)
}

func (x *exporter) agent(ctx context.Context, a *Agent) error {
	if !x.once("agent:" + ref(a.Slug, a.Version)) {
		return nil
	}
	ba, deps, err := agentToBundle(ctx, x.e, a)
	if err != nil {
		return err
	}
	x.b.Agents = append(x.b.Agents, ba)
	x.gens[a.GeneratorSlug] = a.Modal
	for _, p := range deps.prompts {
		if x.once("prompt:" + string(p.Kind) + ":" + ref(p.Slug, p.Version)) {
			x.b.Prompts = append(x.b.Prompts, promptToBundle(p))
		}
	}
	if rf := deps.format; rf != nil && x.once("rf:"+ref(rf.Slug, rf.Version)) {
		x.b.ResponseFormats = append(x.b.ResponseFormats, formatToBundle(rf))
	}
	if deps.fallback != nil {
		return x.agent(ctx, deps.fallback)
	}
	return nil
}

type agentDeps struct {
	prompts  []*Prompt
	format   *ResponseFormatRecord
	fallback *Agent
}

// agentToBundle renders an agent with its references as slug@version, and
// returns the records those references point at.
func agentToBundle(ctx context.Context, e *Engine, a *Agent) (BundleAgent, agentDeps, error) {
	var deps agentDeps
	ba := BundleAgent{Slug: a.Slug, Version: a.Version, Category: a.Category, Modality: a.Modal,
		Generator: a.GeneratorSlug, InlineResponseFormat: a.ResponseFormat, Params: a.Params}
	for _, pr := range []struct {
		id  uuid.UUID
		dst *string
	}{{a.SystemPromptID, &ba.SystemPrompt}, {a.UserTemplateID, &ba.UserTemplate}} {
		if pr.id == uuid.Nil {
			continue
		}
		p, err := e.prompts.GetByID(ctx, pr.id)
		if err != nil {
			return ba, deps, fmt.Errorf("noewright: agent %q prompt %s: %w", a.Slug, pr.id, err)
		}
		*pr.dst = ref(p.Slug, p.Version)
		deps.prompts = append(deps.prompts, p)
	}
	if a.ResponseFormatID != nil && *a.ResponseFormatID != uuid.Nil {
		rf, err := e.responseFormats.GetByID(ctx, *a.ResponseFormatID)
		if err != nil {
			return ba, deps, fmt.Errorf("noewright: agent %q response format: %w", a.Slug, err)
		}
		ba.ResponseFormat, ba.InlineResponseFormat = ref(rf.Slug, rf.Version), nil
		deps.format = rf
	}
	if a.FallbackAgentID != nil && *a.FallbackAgentID != uuid.Nil {
		fb, err := e.agents.GetByID(ctx, *a.FallbackAgentID)
		if err != nil {
			return ba, deps, fmt.Errorf("noewright: agent %q fallback: %w", a.Slug, err)
		}
		ba.Fallback = ref(fb.Slug, fb.Version)
		deps.fallback = fb
	}
	return ba, deps, nil
}

func promptToBundle(p *Prompt) BundlePrompt {
	return BundlePrompt{Slug: p.Slug, Version: p.Version, Kind: p.Kind, Category: p.Category, Body: p.Body,
		Variables: nilIfEmpty(p.Variables), Metadata: nilIfEmptyMap(p.Metadata), Notes: p.Notes}
}

func formatToBundle(rf *ResponseFormatRecord) BundleResponseFormat {
	return BundleResponseFormat{Slug: rf.Slug, Version: rf.Version, Category: rf.Category, Schema: nilIfEmptyMap(rf.Schema), Strict: rf.StrictMode}
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

func nilIfEmptyMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// ---------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------

// importPlan is what Import decided for every record, before writing.
type importPlan struct {
	report  *ImportReport
	bumped  map[string]map[string]int // kind → "slug@v" → new version
	nextVer map[string]int            // kind:slug → next free version, for repeated bumps
	ids     map[string]uuid.UUID      // kind:slug@v (after bumps) → record id
	writes  []func(ex execer) error
	active  []func() error // SetActive calls after commit
	evict   []string       // cache keys to evict after commit
}

func (p *importPlan) key(kind, slug string, v int) string { return kind + ":" + ref(slug, v) }

// resolve returns the version a reference now points at, after any bump.
func (p *importPlan) resolve(kind, slug string, v int) int {
	if nv, ok := p.bumped[kind][ref(slug, v)]; ok {
		return nv
	}
	return v
}

func (s *bundleService) Import(ctx context.Context, b *Bundle, opts ImportOptions) (*ImportReport, error) {
	if b.Format != BundleFormat {
		return nil, fmt.Errorf("%w: format %q, this engine reads %q", ErrInvalidBundle, b.Format, BundleFormat)
	}
	if opts.OnConflict == "" {
		opts.OnConflict = ConflictFail
	}
	if opts.OnConflict != ConflictFail && opts.OnConflict != ConflictBump {
		return nil, fmt.Errorf("noewright: import: unknown conflict policy %q", opts.OnConflict)
	}
	e := s.e
	if !opts.SkipRuntimeChecks {
		if probs := s.Verify(ctx, b, opts); len(probs) > 0 {
			return nil, fmt.Errorf("%w: %s", ErrInvalidBundle, strings.Join(probs, "; "))
		}
	}
	plan := &importPlan{report: &ImportReport{}, bumped: map[string]map[string]int{}, nextVer: map[string]int{}, ids: map[string]uuid.UUID{}}
	now := time.Now()
	owner := opts.Owner

	// decide records one record's outcome. existing is the target's record in
	// bundle form (nil = absent); same reports whether it matches.
	decide := func(kind, slug string, version int, existing any, want any, latest func() (int, error)) (ImportOutcome, int, error) {
		item := ImportItem{Kind: kind, Slug: slug, Version: version}
		defer func() { plan.report.Items = append(plan.report.Items, item) }()
		switch {
		case existing == nil:
			item.Outcome = ImportCreate
			return ImportCreate, version, nil
		case recordHash(existing) == recordHash(want):
			item.Outcome = ImportSkip
			return ImportSkip, version, nil
		case opts.OnConflict == ConflictFail:
			item.Outcome = ImportConflict
			return ImportConflict, version, nil
		}
		nk := kind + ":" + slug
		next := plan.nextVer[nk]
		if next == 0 {
			top, err := latest()
			if err != nil {
				return "", 0, err
			}
			next = top + 1
		}
		plan.nextVer[nk] = next + 1
		if plan.bumped[kind] == nil {
			plan.bumped[kind] = map[string]int{}
		}
		plan.bumped[kind][ref(slug, version)] = next
		item.Outcome, item.NewVersion = ImportBump, next
		return ImportBump, next, nil
	}
	maxVersion := func(table, slug string) func() (int, error) {
		return func() (int, error) {
			var v sql.NullInt64
			err := e.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT MAX(version) FROM %s%s WHERE owner=$1 AND slug=$2`, e.prefix, table), owner, slug).Scan(&v)
			return int(v.Int64), err
		}
	}
	conflicts := 0
	count := func(o ImportOutcome) {
		if o == ImportConflict {
			conflicts++
		}
	}

	// Response formats.
	for _, rf := range b.ResponseFormats {
		var existing any
		cur, err := e.responseFormats.Get(ctx, owner, rf.Slug, rf.Version)
		if err == nil {
			existing = formatToBundle(cur)
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		out, v, err := decide("response_format", rf.Slug, rf.Version, existing, rf, maxVersion("response_formats", rf.Slug))
		if err != nil {
			return nil, err
		}
		count(out)
		k := plan.key("response_format", rf.Slug, v)
		if out == ImportSkip {
			plan.ids[k] = cur.ID
			continue
		}
		rec := &ResponseFormatRecord{ID: uuid.New(), Owner: owner, Slug: rf.Slug, Version: v, Category: rf.Category,
			Schema: rf.Schema, StrictMode: rf.Strict, CreatedAt: now}
		plan.ids[k] = rec.ID
		plan.writes = append(plan.writes, func(ex execer) error { return sqlInsertResponseFormat(ctx, ex, e.prefix, rec) })
	}

	// Prompts (unique per owner, slug, version and kind).
	for _, p := range b.Prompts {
		var existing any
		cur, err := queryPromptKind(ctx, e, owner, p.Slug, p.Version, p.Kind)
		if err == nil {
			existing = promptToBundle(cur)
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		out, v, err := decide("prompt", p.Slug, p.Version, existing, p, maxVersion("prompts", p.Slug))
		if err != nil {
			return nil, err
		}
		count(out)
		k := plan.key("prompt:"+string(p.Kind), p.Slug, v)
		if out == ImportSkip {
			plan.ids[k] = cur.ID
			continue
		}
		rec := &Prompt{ID: uuid.New(), Owner: owner, Slug: p.Slug, Version: v, Kind: p.Kind, Category: p.Category,
			Body: p.Body, Variables: p.Variables, Metadata: p.Metadata, Notes: p.Notes, CreatedAt: now}
		plan.ids[k] = rec.ID
		plan.writes = append(plan.writes, func(ex execer) error { return sqlInsertPrompt(ctx, ex, e.prefix, rec) })
		plan.evict = append(plan.evict, e.cacheKeyOwnedLatest("prompt-latest", owner, p.Slug))
	}

	// Agents, fallbacks before the agents that name them.
	agents, err := orderAgents(b.Agents)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		// Rewrite references to records this import bumped, then map the
		// generator to the local one.
		want := a
		if gen, ok := opts.Generators[a.Generator]; ok {
			want.Generator = gen
		}
		var refErr error
		rewrite := func(kind string, r *string) {
			if *r == "" || refErr != nil {
				return
			}
			slug, v, err := parseRef(*r)
			if err != nil {
				refErr = err
				return
			}
			*r = ref(slug, plan.resolve(kind, slug, v))
		}
		rewrite("prompt", &want.SystemPrompt)
		rewrite("prompt", &want.UserTemplate)
		rewrite("response_format", &want.ResponseFormat)
		rewrite("agent", &want.Fallback)
		if refErr != nil {
			return nil, fmt.Errorf("agent %q: %w", a.Slug, refErr)
		}
		var existing any
		cur, err := e.agents.Get(ctx, owner, a.Slug, a.Version)
		if err == nil {
			ex, _, err := agentToBundle(ctx, e, cur)
			if err != nil {
				return nil, err
			}
			existing = ex
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		out, v, err := decide("agent", a.Slug, a.Version, existing, want, maxVersion("agents", a.Slug))
		if err != nil {
			return nil, err
		}
		count(out)
		k := plan.key("agent", a.Slug, v)
		if out == ImportSkip {
			plan.ids[k] = cur.ID
			continue
		}
		rec := &Agent{ID: uuid.New(), Owner: owner, Slug: a.Slug, Version: v, Category: a.Category, Modal: a.Modality,
			GeneratorSlug: want.Generator, ResponseFormat: a.InlineResponseFormat, Params: a.Params, CreatedAt: now}
		lookup := func(kind, r string) (uuid.UUID, error) {
			slug, ver, err := parseRef(r)
			if err != nil {
				return uuid.Nil, err
			}
			id, ok := plan.ids[plan.key(kind, slug, ver)]
			if !ok {
				return uuid.Nil, fmt.Errorf("%w: agent %q refers to %s %q, which the bundle does not contain", ErrInvalidBundle, a.Slug, strings.Split(kind, ":")[0], r)
			}
			return id, nil
		}
		if want.SystemPrompt != "" {
			if rec.SystemPromptID, err = lookup("prompt:"+string(PromptKindSystem), want.SystemPrompt); err != nil {
				return nil, err
			}
		}
		if want.UserTemplate != "" {
			if rec.UserTemplateID, err = lookup("prompt:"+string(PromptKindUserTemplate), want.UserTemplate); err != nil {
				return nil, err
			}
		}
		if want.ResponseFormat != "" {
			id, err := lookup("response_format", want.ResponseFormat)
			if err != nil {
				return nil, err
			}
			rec.ResponseFormatID = &id
		}
		if want.Fallback != "" {
			id, err := lookup("agent", want.Fallback)
			if err != nil {
				return nil, err
			}
			rec.FallbackAgentID = &id
		}
		plan.ids[k] = rec.ID
		plan.writes = append(plan.writes, func(ex execer) error { return sqlInsertAgent(ctx, ex, e.prefix, rec) })
		plan.evict = append(plan.evict, e.cacheKeyOwnedLatest("agent-latest", owner, a.Slug))
	}

	// Flows.
	for _, f := range b.Flows {
		want := f
		want.Agents = slices.Clone(f.Agents)
		for i, a := range want.Agents {
			if a.Version > 0 {
				want.Agents[i].Version = plan.resolve("agent", a.Agent, a.Version)
			}
		}
		var existing any
		cur, err := e.flows.Get(ctx, owner, f.Slug, f.Version)
		if err == nil {
			existing = flowToBundle(cur)
		} else if !errors.Is(err, ErrNotFound) && !isNotFound(err) {
			return nil, err
		}
		out, v, err := decide("flow", f.Slug, f.Version, existing, want, maxVersion("flows", f.Slug))
		if err != nil {
			return nil, err
		}
		count(out)
		if out == ImportSkip || out == ImportConflict {
			continue
		}
		rec := &FlowRecord{ID: uuid.New(), Owner: owner, Slug: f.Slug, Version: v, Category: f.Category,
			IsActive: opts.Activate, Inputs: f.Inputs, CreatedAt: now}
		for i, a := range want.Agents {
			rec.Agents = append(rec.Agents, FlowAgentEntry{Position: i, AgentSlug: a.Agent, AgentVersion: a.Version,
				OutputKey: a.Output, Stream: a.Stream, GeneratorOverride: a.GeneratorOverride, Params: a.Params,
				RetryMode: a.RetryMode, MaxRetries: a.MaxRetries})
		}
		if len(rec.Agents) == 0 {
			return nil, fmt.Errorf("%w: flow %q has no agents", ErrInvalidBundle, f.Slug)
		}
		if errs := flowProducerConflicts(rec); len(errs) > 0 {
			return nil, fmt.Errorf("%w: flow %q: %w", ErrInvalidBundle, f.Slug, errors.Join(errs...))
		}
		plan.writes = append(plan.writes, func(ex execer) error { return sqlInsertFlowRows(ctx, ex, e.prefix, rec) })
		plan.evict = append(plan.evict, e.cacheKeyOwnedLatest("flow-active", owner, f.Slug))
		if opts.Activate {
			plan.active = append(plan.active, func() error { return e.flows.SetActive(ctx, owner, rec.Slug, rec.Version) })
		}
	}

	// Graphs.
	for _, bg := range b.Graphs {
		want := bg
		want.Graph = cloneGraph(bg.Graph)
		for i, n := range want.Graph.Nodes {
			if n.Kind == NodeAgent && n.Agent != nil && n.Agent.AgentVersion > 0 {
				want.Graph.Nodes[i].Agent.AgentVersion = plan.resolve("agent", n.Agent.AgentSlug, n.Agent.AgentVersion)
			}
		}
		var existing any
		cur, err := e.graphs.Get(ctx, owner, bg.Slug, bg.Version)
		if err == nil {
			existing = graphToBundle(cur)
		} else if !errors.Is(err, ErrNotFound) && !isNotFound(err) {
			return nil, err
		}
		out, v, err := decide("graph", bg.Slug, bg.Version, existing, want, maxVersion("graphs", bg.Slug))
		if err != nil {
			return nil, err
		}
		count(out)
		if out == ImportSkip || out == ImportConflict {
			continue
		}
		g := want.Graph
		g.Slug = bg.Slug
		if _, err := e.compileGraphOpts(ctx, owner, g, opts.SkipRuntimeChecks); err != nil {
			return nil, fmt.Errorf("graph %q: %w", bg.Slug, err)
		}
		for _, n := range g.Nodes {
			if n.Agent != nil && len(n.Agent.Overrides) > 0 {
				return nil, fmt.Errorf("%w: graph %q node %q carries Agent.Overrides, which hold secrets", ErrInvalidBundle, bg.Slug, n.ID)
			}
		}
		rec := &GraphRecord{ID: uuid.New(), Owner: owner, Slug: bg.Slug, Version: v, Category: bg.Category,
			IsActive: opts.Activate, Graph: g, CreatedAt: now}
		plan.writes = append(plan.writes, func(ex execer) error { return sqlInsertGraph(ctx, ex, e.prefix, rec) })
		plan.evict = append(plan.evict, e.cacheKeyOwnedLatest("graph-active", owner, bg.Slug))
		if opts.Activate {
			plan.active = append(plan.active, func() error { return e.graphs.SetActive(ctx, owner, rec.Slug, rec.Version) })
		}
	}

	if opts.SkipRuntimeChecks {
		plan.report.Warnings = append(plan.report.Warnings, s.Verify(ctx, b, opts)...)
	}
	if conflicts > 0 {
		return plan.report, fmt.Errorf("%w: %d record(s) differ from what is already there; use OnConflict bump to import them as new versions", ErrBundleConflict, conflicts)
	}
	if opts.DryRun || len(plan.writes) == 0 {
		return plan.report, nil
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return plan.report, err
	}
	defer tx.Rollback()
	for _, w := range plan.writes {
		if err := w(tx); err != nil {
			return plan.report, fmt.Errorf("noewright: import: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return plan.report, fmt.Errorf("noewright: import: %w", err)
	}
	plan.report.Written = true
	for _, k := range plan.evict {
		cacheDelete(ctx, e.cache, k)
	}
	for _, act := range plan.active {
		if err := act(); err != nil {
			return plan.report, fmt.Errorf("noewright: import: activate: %w", err)
		}
	}
	return plan.report, nil
}

func isNotFound(err error) bool {
	var nf *NotFoundError
	return errors.As(err, &nf)
}

// queryPromptKind reads one prompt by owner, slug, version and kind.
func queryPromptKind(ctx context.Context, e *Engine, owner, slug string, version int, kind PromptKind) (*Prompt, error) {
	row := e.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %sprompts WHERE owner=$1 AND slug=$2 AND version=$3 AND kind=$4`,
		promptColumns, e.prefix), owner, slug, version, string(kind))
	p, err := scanPrompt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// orderAgents puts every agent after the agent it falls back to.
func orderAgents(in []BundleAgent) ([]BundleAgent, error) {
	byRef := map[string]BundleAgent{}
	for _, a := range in {
		byRef[ref(a.Slug, a.Version)] = a
	}
	state := map[string]int{}
	var out []BundleAgent
	var visit func(a BundleAgent) error
	visit = func(a BundleAgent) error {
		k := ref(a.Slug, a.Version)
		switch state[k] {
		case 1:
			return fmt.Errorf("%w: agent fallbacks form a loop through %q", ErrInvalidBundle, k)
		case 2:
			return nil
		}
		state[k] = 1
		if fb, ok := byRef[a.Fallback]; ok && a.Fallback != "" {
			if err := visit(fb); err != nil {
				return err
			}
		}
		state[k] = 2
		out = append(out, a)
		return nil
	}
	for _, a := range in {
		if err := visit(a); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// cloneGraph copies a graph deeply enough that changing a node's agent
// version does not change the original.
func cloneGraph(g Graph) Graph {
	c := g
	c.Nodes = slices.Clone(g.Nodes)
	for i, n := range c.Nodes {
		if n.Agent != nil {
			a := *n.Agent
			c.Nodes[i].Agent = &a
		}
	}
	return c
}

// ---------------------------------------------------------------------------
// Verify
// ---------------------------------------------------------------------------

func (s *bundleService) Verify(ctx context.Context, b *Bundle, opts ImportOptions) []string {
	e := s.e
	var probs []string
	registered := map[string]bool{}
	for _, g := range e.Generators() {
		registered[g] = true
	}
	for _, g := range b.Requires.Generators {
		local := g.Slug
		if m, ok := opts.Generators[g.Slug]; ok {
			local = m
		}
		if !registered[local] {
			if local == g.Slug {
				probs = append(probs, fmt.Sprintf("generator %q is not registered; register it or map it to one that is", g.Slug))
			} else {
				probs = append(probs, fmt.Sprintf("generator %q maps to %q, which is not registered", g.Slug, local))
			}
		}
	}
	for _, f := range b.Requires.Funcs {
		if _, ok := e.lookupFunc(f); !ok {
			probs = append(probs, fmt.Sprintf("function %q is not registered; call Engine.RegisterFunc(%q, ...)", f, f))
		}
	}
	if b.Requires.Evaluator && e.Evaluator() == nil {
		probs = append(probs, "a graph has decide nodes but no Evaluator is configured")
	}
	for _, bg := range b.Graphs {
		g := bg.Graph
		g.Slug = bg.Slug
		if _, err := e.compileGraphOpts(ctx, opts.Owner, g, true); err != nil {
			probs = append(probs, fmt.Sprintf("graph %q: %v", bg.Slug, err))
		}
	}
	return probs
}
