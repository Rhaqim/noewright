package main

// bundle.go — the export, import and verify commands.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"

	"github.com/rhaqim/noewright"
	"github.com/rhaqim/noewright/schema"
)

func openEngine(cmd *cobra.Command) (*noewright.Engine, func(), error) {
	db, err := openDB(cmd)
	if err != nil {
		return nil, nil, err
	}
	prefix, _ := cmd.Flags().GetString("prefix")
	e, err := noewright.New(noewright.Config{DB: db, Dialect: noewright.DialectPostgres, SchemaPrefix: prefix, Generators: cliGenerators()})
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("engine: %w", err)
	}
	return e, func() { db.Close() }, nil
}

// parseRoot reads "kind:slug" or "kind:slug@version".
func parseRoot(s string) (noewright.BundleRoot, error) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || rest == "" {
		return noewright.BundleRoot{}, fmt.Errorf("%q: write roots as kind:slug or kind:slug@version (kind is graph, flow or agent)", s)
	}
	r := noewright.BundleRoot{Kind: kind, Slug: rest}
	if slug, v, ok := strings.Cut(rest, "@"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return r, fmt.Errorf("%q: the version after @ must be a number from 1", s)
		}
		r.Slug, r.Version = slug, n
	}
	return r, nil
}

func exportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export <kind:slug[@version]>...",
		Short: "Write a bundle of graphs, flows or agents and everything they use",
		Long: "Export follows every reference from the roots you name (graph:support, flow:triage-turn@2, agent:writer)\n" +
			"and writes one bundle file. Version omitted = the active graph or flow, or the latest agent.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var roots []noewright.BundleRoot
			for _, a := range args {
				r, err := parseRoot(a)
				if err != nil {
					return err
				}
				roots = append(roots, r)
			}
			e, done, err := openEngine(cmd)
			if err != nil {
				return err
			}
			defer done()
			owner, _ := cmd.Flags().GetString("owner")
			b, err := e.Bundles().Export(context.Background(), owner, roots...)
			if err != nil {
				return err
			}
			b.Name, _ = cmd.Flags().GetString("name")
			data, err := b.Marshal()
			if err != nil {
				return err
			}
			out, _ := cmd.Flags().GetString("output")
			if out == "" || out == "-" {
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			if err := os.WriteFile(out, data, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Wrote %s: %d prompts, %d agents, %d flows, %d graphs.\n",
				out, len(b.Prompts), len(b.Agents), len(b.Flows), len(b.Graphs))
			return nil
		},
	}
	cmd.Flags().String("owner", "", "owner scope to export from (default global)")
	cmd.Flags().StringP("output", "o", "", "file to write (default stdout)")
	cmd.Flags().String("name", "", "a name for the bundle")
	cmd.Flags().String("prefix", "loom_", "Table name prefix")
	return cmd
}

func readBundle(path string) (*noewright.Bundle, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 16<<20+1))
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	return noewright.ParseBundle(data)
}

// mapFlag reads repeated --map from=to flags.
func mapFlag(cmd *cobra.Command) (map[string]string, error) {
	pairs, _ := cmd.Flags().GetStringArray("map")
	m := map[string]string{}
	for _, p := range pairs {
		from, to, ok := strings.Cut(p, "=")
		if !ok || from == "" || to == "" {
			return nil, fmt.Errorf("--map %q: write it as bundle-generator=local-generator", p)
		}
		m[from] = to
	}
	return m, nil
}

func printImport(w io.Writer, rep *noewright.ImportReport) {
	if rep == nil {
		return
	}
	fmt.Fprintf(w, "%-16s %-28s %-8s %s\n", "KIND", "RECORD", "OUTCOME", "")
	for _, it := range rep.Items {
		note := ""
		if it.Outcome == noewright.ImportBump {
			note = fmt.Sprintf("as version %d", it.NewVersion)
		}
		fmt.Fprintf(w, "%-16s %-28s %-8s %s\n", it.Kind, fmt.Sprintf("%s@%d", it.Slug, it.Version), it.Outcome, note)
	}
	for _, warn := range rep.Warnings {
		fmt.Fprintln(w, "note:", warn)
	}
}

func importCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <bundle.yaml>",
		Short: "Load a bundle into the database",
		Long: "Import creates records the database does not have, skips identical ones, and stops on any that differ\n" +
			"(--on-conflict bump imports those as new versions instead). Everything is written in one transaction.\n" +
			"Generators and Go functions are registered by your app, not the CLI: map generators with --map, and\n" +
			"run Bundles().Verify in your app at startup.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := readBundle(args[0])
			if err != nil {
				return err
			}
			gens, err := mapFlag(cmd)
			if err != nil {
				return err
			}
			e, done, err := openEngine(cmd)
			if err != nil {
				return err
			}
			defer done()
			opts := noewright.ImportOptions{Generators: gens, SkipRuntimeChecks: true}
			opts.Owner, _ = cmd.Flags().GetString("owner")
			policy, _ := cmd.Flags().GetString("on-conflict")
			opts.OnConflict = noewright.ConflictPolicy(policy)
			opts.Activate, _ = cmd.Flags().GetBool("activate")
			opts.DryRun, _ = cmd.Flags().GetBool("dry-run")
			rep, err := e.Bundles().Import(context.Background(), b, opts)
			printImport(cmd.OutOrStdout(), rep)
			if err != nil {
				return err
			}
			switch {
			case opts.DryRun:
				fmt.Fprintln(cmd.OutOrStdout(), "Dry run: nothing was written.")
			case rep.Written:
				fmt.Fprintln(cmd.OutOrStdout(), "Imported.")
			default:
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing to import: every record is already there.")
			}
			return nil
		},
	}
	cmd.Flags().String("owner", "", "owner scope to import into (default global)")
	cmd.Flags().StringArray("map", nil, "map a bundle generator to a local one: --map chat=openai-mini (repeatable)")
	cmd.Flags().String("on-conflict", "fail", "fail or bump")
	cmd.Flags().Bool("activate", false, "make imported flows and graphs the active version")
	cmd.Flags().Bool("dry-run", false, "show what would happen without writing")
	cmd.Flags().String("prefix", "loom_", "Table name prefix")
	return cmd
}

// isBundleFile reports whether path holds a bundle rather than a test plan.
func isBundleFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, _ := io.ReadFull(f, head)
	return strings.Contains(string(head[:n]), "noewright: bundle/")
}

// testBundle runs a bundle's tests offline against the echo generator.
func testBundle(cmd *cobra.Command, path string) error {
	b, err := readBundle(path)
	if err != nil {
		return err
	}
	if len(b.Tests) == 0 {
		return fmt.Errorf("%s has no tests", path)
	}
	if len(b.Requires.Funcs) > 0 || b.Requires.Evaluator {
		return fmt.Errorf("this bundle's graphs need things only your app has (functions %v, evaluator %v); "+
			"run Bundles().Test in your app instead", b.Requires.Funcs, b.Requires.Evaluator)
	}
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:test_"+uuid.NewString()+"?mode=memory&cache=shared")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		return err
	}
	e, err := noewright.New(noewright.Config{DB: db, Dialect: noewright.DialectSQLite, Generators: cliGenerators()})
	if err != nil {
		return err
	}
	gens := map[string]string{}
	for _, g := range b.Requires.Generators {
		gens[g.Slug] = "echo"
	}
	if _, err := e.Bundles().Import(ctx, b, noewright.ImportOptions{Generators: gens, Activate: true}); err != nil {
		return err
	}
	results, err := e.Bundles().Test(ctx, b, noewright.BundleTestOptions{})
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	failed := 0
	for _, r := range results {
		status := "PASS"
		if !r.Evidence.Passed {
			status, failed = "FAIL", failed+1
		}
		fmt.Fprintf(w, "%s  %s\n", status, r.Evidence.Test)
		for _, f := range r.Evidence.Failures {
			fmt.Fprintln(w, "      ", f)
		}
	}
	fmt.Fprintf(w, "%d of %d tests passed (echo generator, offline).\n", len(results)-failed, len(results))
	if failed > 0 {
		return fmt.Errorf("%d test(s) failed", failed)
	}
	return nil
}

func schemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema for bundle files",
		Long:  "Point your editor's YAML support at this schema to get completion and checks while writing a bundle.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(noewright.BundleSchema())
			return err
		},
	}
}

func verifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <bundle.yaml>",
		Short: "Check a bundle without a database",
		Long: "Verify checks the format and digest, then loads the bundle into a throwaway in-memory database,\n" +
			"which checks every record, reference and graph. It lists what the importing app must provide.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := readBundle(args[0])
			if err != nil {
				return err
			}
			ctx := context.Background()
			db, err := sql.Open("sqlite", "file:verify_"+uuid.NewString()+"?mode=memory&cache=shared")
			if err != nil {
				return err
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
				return err
			}
			e, err := noewright.New(noewright.Config{DB: db, Dialect: noewright.DialectSQLite, Generators: cliGenerators()})
			if err != nil {
				return err
			}
			if _, err := e.Bundles().Import(ctx, b, noewright.ImportOptions{SkipRuntimeChecks: true}); err != nil {
				if errors.Is(err, noewright.ErrBundleConflict) {
					return fmt.Errorf("the bundle contains conflicting copies of a record: %w", err)
				}
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "OK: %d prompts, %d response formats, %d agents, %d flows, %d graphs.\n",
				len(b.Prompts), len(b.ResponseFormats), len(b.Agents), len(b.Flows), len(b.Graphs))
			if b.Digest == "" {
				fmt.Fprintln(w, "note: the bundle has no digest, so edits cannot be detected.")
			}
			var needs []string
			for _, g := range b.Requires.Generators {
				needs = append(needs, fmt.Sprintf("generator %q (%s)", g.Slug, g.Modality))
			}
			for _, f := range b.Requires.Funcs {
				needs = append(needs, fmt.Sprintf("function %q", f))
			}
			if b.Requires.Evaluator {
				needs = append(needs, "an Evaluator")
			}
			if len(needs) > 0 {
				fmt.Fprintln(w, "The importing app must provide:")
				for _, n := range needs {
					fmt.Fprintln(w, "  -", n)
				}
			}
			return nil
		},
	}
}
