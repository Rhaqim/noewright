package engine

// bundle_tests.go — running a bundle's tests and recording their evidence.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

func (s *bundleService) Test(ctx context.Context, b *Bundle, opts BundleTestOptions) ([]BundleTestResult, error) {
	prior := map[string]BundleEvidence{}
	for _, ev := range b.Evidence {
		prior[ev.Test] = ev
	}
	var results []BundleTestResult
	for _, t := range b.Tests {
		if len(opts.Only) > 0 && !slices.Contains(opts.Only, t.Name) {
			continue
		}
		ev, texts, err := s.runTest(ctx, t, opts)
		if err != nil {
			return results, fmt.Errorf("noewright: bundle test %q: %w", t.Name, err)
		}
		res := BundleTestResult{Evidence: ev, Texts: texts}
		if before, ok := prior[t.Name]; ok {
			for k, h := range ev.Outputs {
				if old, had := before.Outputs[k]; had && old != h {
					res.Changed = append(res.Changed, k)
				}
			}
			sort.Strings(res.Changed)
		}
		results = append(results, res)
	}
	return results, nil
}

// runTest runs one test in a fresh session. A failed expectation is recorded
// on the evidence; an error means the test could not run at all.
func (s *bundleService) runTest(ctx context.Context, t BundleTest, opts BundleTestOptions) (BundleEvidence, map[string]string, error) {
	e := s.e
	ev := BundleEvidence{Test: t.Name, At: time.Now().UTC().Format(time.RFC3339), Outputs: map[string]string{}, Priced: true,
		Model: opts.GeneratorOverride}
	texts := map[string]string{}
	slug, version, err := parseRef(t.Graph)
	if err != nil {
		return ev, nil, err
	}
	sess := &Session{PlatformID: "bundle-test:" + t.Name, Tags: []string{"bundle-test"}, State: State{Vars: maps.Clone(t.Vars)}}
	if err := e.sessions.Create(ctx, sess); err != nil {
		return ev, nil, err
	}
	fail := func(step int, format string, args ...any) {
		ev.Failures = append(ev.Failures, fmt.Sprintf("step %d: ", step+1)+fmt.Sprintf(format, args...))
	}
	for i, st := range t.Steps {
		inputs := maps.Clone(t.Inputs)
		if inputs == nil {
			inputs = map[string]any{}
		}
		maps.Copy(inputs, st.Inputs)
		var mu sync.Mutex
		outputs := map[string]string{} // node → output text, every kind of node
		req := GraphRequest{Inputs: inputs, Overrides: opts.Overrides, Params: opts.Params, GeneratorOverride: opts.GeneratorOverride,
			OnNode: func(ne NodeEvent) {
				if ne.Kind == NodeDone {
					mu.Lock()
					outputs[ne.Node] = ne.Text
					mu.Unlock()
				}
			}}
		switch {
		case st.Select != "":
			req.Action = &Action{Kind: ActionSelect, Payload: map[string]any{"option_id": st.Select}}
		case st.Say != "":
			req.Action = &Action{Kind: ActionFreeText, Payload: map[string]any{"text": st.Say}}
		}
		run, err := e.RunGraphBySlug(ctx, sess, opts.Owner, slug, version, req)
		if run == nil {
			fail(i, "the run did not start: %v", err)
			break
		}
		if err != nil {
			fail(i, "%v", err)
		}
		ev.USD += run.Cost.TotalUSD
		if !run.Cost.PricingConfigured {
			ev.Priced = false
		}
		for node, text := range outputs {
			k := fmt.Sprintf("step%d.%s", i+1, node)
			sum := sha256.Sum256([]byte(text))
			ev.Outputs[k] = hex.EncodeToString(sum[:])
			texts[k] = text
		}

		x := st.Expect
		switch {
		case x.Waiting == "done" && run.Waiting != "":
			fail(i, "expected the run to finish, it waits at %q", run.Waiting)
		case x.Waiting != "" && x.Waiting != "done" && run.Waiting != x.Waiting:
			fail(i, "expected to wait at %q, waits at %q", x.Waiting, run.Waiting)
		}
		ran := func(node string) bool { return slices.Contains(run.Ran, node) }
		for _, n := range x.Ran {
			if !ran(n) {
				fail(i, "expected %q to run", n)
			}
		}
		for _, n := range x.Skipped {
			if ran(n) {
				fail(i, "expected %q not to run", n)
			}
		}
		for node, want := range x.Contains {
			if !strings.Contains(outputs[node], want) {
				fail(i, "%q's output does not contain %q", node, want)
			}
		}
		for node, limit := range x.MaxLength {
			if n := utf8.RuneCountInString(outputs[node]); n > limit {
				fail(i, "%q's output is %d characters, over %d", node, n, limit)
			}
		}
		if x.NoErrors && len(run.Errors) > 0 {
			keys := slices.Sorted(maps.Keys(run.Errors))
			fail(i, "nodes failed: %s (%v)", strings.Join(keys, ", "), run.Errors[keys[0]])
		}
	}
	sort.Strings(ev.Failures)
	ev.Passed = len(ev.Failures) == 0
	if len(ev.Outputs) == 0 {
		ev.Outputs = nil
	}
	return ev, texts, nil
}

// SetEvidence replaces the bundle's evidence with these results. Marshal the
// bundle afterwards to write it; the digest does not cover evidence.
func (b *Bundle) SetEvidence(results []BundleTestResult) {
	b.Evidence = nil
	for _, r := range results {
		b.Evidence = append(b.Evidence, r.Evidence)
	}
}
