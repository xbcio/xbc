// Command plugin-migration-inventory generates the AST-backed Everything Is
// a Plugin migration inventory and reports its blocker count.
//
// The Everything Is a Plugin migration is complete; this tool now exists
// solely as a regression guard (see TestArchPluginMigrationHasNoBlockers),
// run with -stdout -require-zero so no committed baseline file is needed.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("plugin-migration-inventory", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stdout := flags.Bool("stdout", false, "write the generated inventory to stdout")
	requireZero := flags.Bool("require-zero", false, "fail when any migration blocker remains")
	rootFlag := flags.String("root", "", "repository root containing go.work (normally auto-detected)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	if !*stdout && !*requireZero {
		fmt.Fprintln(os.Stderr, "at least one of -stdout or -require-zero is required")
		return 2
	}

	root, err := resolveRepositoryRoot(*rootFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "locate repository: %v\n", err)
		return 1
	}
	inventory, err := generateInventory(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate inventory: %v\n", err)
		return 1
	}
	generated, err := marshalInventory(inventory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode inventory: %v\n", err)
		return 1
	}

	if *requireZero && inventory.Summary.MigrationBlockers != 0 {
		fmt.Fprintf(
			os.Stderr,
			"plugin migration inventory has %d blockers (retired APIs=%d, context retention=%d, noncanonical accessors=%d, generic order refs=%d, Web literal order refs=%d)\n",
			inventory.Summary.MigrationBlockers,
			inventory.Summary.RetiredAPIUses,
			inventory.Summary.ContextRetentions,
			inventory.Summary.NoncanonicalDefinitionAccessors,
			inventory.Summary.GenericOrderReferences,
			inventory.Summary.WebLiteralOrderReferences,
		)
		return 1
	}

	if *stdout {
		if _, err := os.Stdout.Write(generated); err != nil {
			fmt.Fprintf(os.Stderr, "write stdout: %v\n", err)
			return 1
		}
	}
	return 0
}

func displayPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

func resolveRepositoryRoot(explicit string) (string, error) {
	if explicit != "" {
		root, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
			return "", fmt.Errorf("%s does not contain go.work: %w", root, err)
		}
		return filepath.Clean(root), nil
	}
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.work")); err == nil {
			return filepath.Clean(current), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no go.work found at or above the current directory")
		}
		current = parent
	}
}
