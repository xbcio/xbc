// Command plugin-migration-inventory generates and checks the AST-backed
// Everything Is a Plugin migration inventory.
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultOutputPath = ".claude/migration/everything-plugin/inventory.json"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("plugin-migration-inventory", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	write := flags.Bool("write", false, "write the generated inventory to -output")
	check := flags.Bool("check", false, "fail when -output differs from the generated inventory")
	stdout := flags.Bool("stdout", false, "write the generated inventory to stdout")
	requireZero := flags.Bool("require-zero", false, "fail when any migration blocker remains")
	output := flags.String("output", defaultOutputPath, "checked inventory path, relative to the repository root")
	rootFlag := flags.String("root", "", "repository root containing go.work (normally auto-detected)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	modes := 0
	for _, enabled := range []bool{*write, *check, *stdout} {
		if enabled {
			modes++
		}
	}
	if modes != 1 {
		fmt.Fprintln(os.Stderr, "exactly one of -write, -check, or -stdout is required")
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

	outputPath := *output
	if !filepath.IsAbs(outputPath) {
		outputPath = filepath.Join(root, filepath.FromSlash(outputPath))
	}
	switch {
	case *stdout:
		if _, err := os.Stdout.Write(generated); err != nil {
			fmt.Fprintf(os.Stderr, "write stdout: %v\n", err)
			return 1
		}
	case *write:
		if err := writeAtomically(outputPath, generated); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", displayPath(root, outputPath), err)
			return 1
		}
		fmt.Printf("wrote %s (%d migration blockers)\n", displayPath(root, outputPath), inventory.Summary.MigrationBlockers)
	case *check:
		checked, err := os.ReadFile(outputPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read checked inventory %s: %v\n", displayPath(root, outputPath), err)
			return 1
		}
		if !bytes.Equal(checked, generated) {
			wantHash := sha256.Sum256(checked)
			gotHash := sha256.Sum256(generated)
			line, want, got := firstDifferentLine(checked, generated)
			fmt.Fprintf(
				os.Stderr,
				"plugin migration inventory drifted: %s\nchecked sha256:   %x\ngenerated sha256: %x\nfirst difference at line %d:\n  checked:   %s\n  generated: %s\nregenerate with: go run ./scripts/plugin-migration-inventory -write\n",
				displayPath(root, outputPath), wantHash, gotHash, line, want, got,
			)
			return 1
		}
		fmt.Printf("%s is current (%d migration blockers)\n", displayPath(root, outputPath), inventory.Summary.MigrationBlockers)
	}
	return 0
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

func writeAtomically(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".inventory-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func displayPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

func firstDifferentLine(left, right []byte) (line int, leftLine, rightLine string) {
	leftLines := strings.Split(string(left), "\n")
	rightLines := strings.Split(string(right), "\n")
	limit := len(leftLines)
	if len(rightLines) < limit {
		limit = len(rightLines)
	}
	for index := 0; index < limit; index++ {
		if leftLines[index] != rightLines[index] {
			return index + 1, leftLines[index], rightLines[index]
		}
	}
	if len(leftLines) != len(rightLines) {
		leftValue, rightValue := "<missing>", "<missing>"
		if limit < len(leftLines) {
			leftValue = leftLines[limit]
		}
		if limit < len(rightLines) {
			rightValue = rightLines[limit]
		}
		return limit + 1, leftValue, rightValue
	}
	return 0, "<none>", "<none>"
}
