// Command ultrac is the Go engine's entry point.
//
// # Status
//
// Phase 1 skeleton. Nothing is wired yet: the agent loop, the provider
// implementations, the built-in tools and the NDJSON transport all land in
// later phases. What exists now is the contract layer and a consumer that
// compiles against it, so the interfaces are exercised by the build rather
// than only by review.
//
// # Argument parsing
//
// Hand-rolled for now. The TypeScript side has 193 subcommands and needs a
// real router; this binary has none yet, and adding a framework before it has
// a shape worth organising would be guessing. The subcommand tree gets
// reverse-engineered from the existing implementation when it is ported.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"ultrac/internal/config"
	"ultrac/internal/contract/provider"
	"ultrac/internal/contract/tool"
	"ultrac/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) > 0 && (args[0] == "--version" || args[0] == "-v"):
		fmt.Fprintln(stdout, version.String())
		return 0

	case len(args) > 0 && (args[0] == "--help" || args[0] == "-h"):
		usage(stdout)
		return 0

	case len(args) > 0:
		fmt.Fprintf(stderr, "ultrac: unknown argument %q\n\n", args[0])
		usage(stderr)
		return 2

	default:
		usage(stdout)
		return 0
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "ultrac %s — Go engine skeleton\n\n", version.Get().Version)
	fmt.Fprintln(w, "No commands are wired yet. This build proves the contract layer")
	fmt.Fprintln(w, "compiles and that self-registration is reachable.")
	fmt.Fprintln(w)

	fmt.Fprintf(w, "  provider kinds registered: %s\n", listOrNone(provider.Kinds()))
	fmt.Fprintf(w, "  built-in tools registered: %s\n", listOrNone(tool.BuiltinNames()))
	fmt.Fprintf(w, "  provider env keys honoured: %s\n", strings.Join(config.ProviderEnvKeys, ", "))
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ultrac [--help|--version]")
}
func listOrNone(items []string) string {
	if len(items) == 0 {
		return "(none yet)"
	}
	return strings.Join(items, ", ")
}
