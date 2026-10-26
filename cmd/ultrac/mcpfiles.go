package main

// mcpfiles.go — `ultrac mcp import <path>`
// (docs/开发计划/新功能计划/mcp-plan.md P5).
//
// # Why a command and not a second configuration source
//
// settings.json is the only place a server is declared (D1). The MCP
// ecosystem's convention is a `.mcp.json` beside the project, and a user who
// already has one should not have to retype it — but reading that file at
// boot would make Ultrac a program that guesses where its configuration
// lives, which is the exact failure D11 removed for providers. So the file is
// read once, on demand, by this command: an import is a user decision, not a
// parse chain.
//
// # Where the entries land, and what a same-named entry means
//
// The USER layer, wholesale, per server name. Same choice as
// add_custom_model and for the same reason (settings-unify-plan.md D8): the
// layer that is cwd-independent is the one whose contents survive opening the
// desktop from another directory. A same-named entry already there is
// replaced and the replacement is printed — a silent drop of the previous
// definition (its disabledTools included) is data loss nobody gets to read
// about afterwards.
//
// # All-or-nothing on a malformed entry
//
// An entry that does not decode into the typed shape is refused by name and
// nothing is written. The point of importing is to make servers available; a
// malformed one is what the user would otherwise have to debug at boot, and
// the file it came from is still right there to fix.
//
// An entry that decodes but this build cannot serve — an `http` transport, a
// stdio entry with no command, a stdio entry carrying a url — IS written, and
// reported as unusable. Two reasons: the definition is the user's data (a
// later build may serve it), and the judgement is mcp.FromConfig's, the one
// boot and `ultrac config` already apply, so it is stated now rather than
// discovered later.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"ultrac/internal/config"
	"ultrac/internal/mcp"
)

const mcpImportUsage = `ultrac mcp import <path>

  Copy the mcpServers section of a file (a Claude-style .mcp.json, or any
  settings.json) into the user settings layer's mcpServers section.

Flags:
      --dry-run    print what would be written and change nothing
`

// runMCP dispatches `ultrac mcp <subcommand>`.
//
// One subcommand today. The shape is the dispatch's rather than a flag on
// `ultrac config`, because importing writes configuration and `config` only
// reads it — the same separation `sessions` keeps from `probe`.
func runMCP(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(stderr, mcpImportUsage)
		return 2
	}
	switch args[0] {
	case "import":
		return runMCPImport(args[1:], stdout, stderr)
	case "help":
		fmt.Fprint(stdout, mcpImportUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "ultrac mcp: unknown subcommand %q\n\n%s", args[0], mcpImportUsage)
		return 2
	}
}

// runMCPImport reads one file, merges its servers into the user layer, and
// reports every name it touched.
func runMCPImport(args []string, stdout, stderr io.Writer) int {
	dryRun := false
	path := ""
	for _, a := range args {
		switch {
		case a == "--dry-run":
			dryRun = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(stderr, "ultrac mcp import: unknown flag %q\n\n%s", a, mcpImportUsage)
			return 2
		case path == "":
			path = a
		default:
			// One file at a time, said out loud: importing two overlapping
			// definitions in one command would need a precedence rule nobody
			// asked for.
			fmt.Fprintf(stderr, "ultrac mcp import: one file at a time (also got %q)\n", a)
			return 2
		}
	}
	if path == "" {
		fmt.Fprintf(stderr, "ultrac mcp import: needs a file\n\n%s", mcpImportUsage)
		return 2
	}

	servers, err := readMCPServerFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "ultrac mcp import: %v\n", err)
		return 1
	}
	names := config.MCPServerNames(servers)
	if len(names) == 0 {
		fmt.Fprintf(stderr, "ultrac mcp import: %s declares no servers (mcpServers is empty)\n", path)
		return 1
	}

	var broken []string
	for _, name := range names {
		if servers[name].Broken() {
			broken = append(broken, name)
		}
	}
	if len(broken) > 0 {
		fmt.Fprintf(stderr, "ultrac mcp import: %s: cannot decode %s\n", path, strings.Join(broken, ", "))
		fmt.Fprintln(stderr, "  nothing was written — fix the entry in the source file and run this again.")
		return 1
	}

	// Well-formed but unusable: kept and flagged. mcp.FromConfig is the same
	// call `ultrac config` and the boot make, so the three cannot disagree
	// about what "unusable" means.
	unusable := make(map[string]string, len(names))
	for _, name := range names {
		if _, err := mcp.FromConfig(name, servers[name]); err != nil {
			unusable[name] = err.Error()
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		// Not fatal: the user layer does not hang off a working directory.
		cwd = ""
	}
	settingsPath := config.SourceFile(config.SourceUser, cwd)
	replaced := existingServerNames(cwd, names)

	actionOf := func(name string) string {
		if replaced[name] {
			return "replaced"
		}
		return "added"
	}

	// The report is printed before the write and after it is known to be
	// possible — a preview the user could not have got any other way
	// (--dry-run) and the record of what just changed are the same text.
	fmt.Fprintf(stdout, "from: %s\n", path)
	for _, name := range names {
		line(stdout, "  %-16s %-9s %s", name, actionOf(name), mcpCommandLine(servers[name]))
		if detail := mcpEntryDetails(servers[name]); detail != "" {
			line(stdout, "  %-16s %-9s %s", "", "", detail)
		}
		if reason, bad := unusable[name]; bad {
			line(stdout, "  %-16s %-9s unusable: %s", "", "", reason)
		}
	}

	added, rep := 0, 0
	for _, name := range names {
		if replaced[name] {
			rep++
		} else {
			added++
		}
	}

	if dryRun {
		fmt.Fprintf(stdout, "\ndry run: %d added, %d replaced — nothing written to %s\n", added, rep, settingsPath)
		return 0
	}
	r := config.NewFileResolver()
	err = r.Update(context.Background(), config.SourceUser, cwd, func(s *config.Settings) error {
		if s.MCPServers == nil {
			s.MCPServers = make(map[string]config.MCPServerConfig, len(names))
		}
		for _, name := range names {
			// Wholesale, the merge rule's semantics one level down (I3): a
			// half-merged entry would be a configuration nobody wrote.
			s.MCPServers[name] = servers[name]
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "ultrac mcp import: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "\n%d added, %d replaced in %s\n", added, rep, settingsPath)
	// The one thing the file cannot say: this process is not the one that
	// will use the change. Membership is decided at boot (§3.1).
	fmt.Fprintln(stdout, "restart the kernel to pick the new servers up (the tool set is fixed at boot).")
	if len(unusable) > 0 {
		bad := make([]string, 0, len(unusable))
		for name := range unusable {
			bad = append(bad, name)
		}
		slices.Sort(bad)
		fmt.Fprintf(stderr, "warning: written but unusable: %s\n", strings.Join(bad, ", "))
	}
	return 0
}

// line writes one padded report row without the trailing blanks the padding
// leaves on a short row. The columns are for reading, not for a diff.
func line(w io.Writer, format string, args ...any) {
	fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf(format, args...), " "))
}

// readMCPServerFile reads one file's mcpServers section.
//
// The section is required by name rather than guessed: a JSON object whose
// keys happen to look like server names is not a definition, and inferring
// one is how an import silently writes the wrong thing. A bare map with no
// wrapper is therefore an error that says what was expected.
//
// Every entry goes through config.MCPServerConfig's decoder, so the
// snake_case spellings an MCP README uses (startup_timeout_seconds,
// disabled_tools) are accepted on the way in and written back as camelCase
// (I5) — the import is one more writer, and there is only one spelling.
func readMCPServerFile(path string) (map[string]config.MCPServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("%s: not a JSON object: %w", path, err)
	}
	section, ok := top["mcpServers"]
	if !ok {
		hint := "expected \"mcpServers\" at the top level"
		if _, looksLikeServers := top["servers"]; looksLikeServers {
			// VS Code spells it "servers". Naming the difference is cheaper
			// than a user staring at a file that "obviously" has servers in it.
			hint += ` (this file uses "servers"; rename it to "mcpServers")`
		}
		return nil, fmt.Errorf("%s: no mcpServers section — %s", path, hint)
	}
	var servers map[string]config.MCPServerConfig
	if err := json.Unmarshal(section, &servers); err != nil {
		return nil, fmt.Errorf("%s: mcpServers is not an object of server definitions: %w", path, err)
	}
	return servers, nil
}

// existingServerNames reports which of the names the user layer already
// declares, so the report can say "replaced" before the write rather than
// after it.
//
// A read failure means "nothing known to be there": the write that follows is
// the authority, and it reports its own error if the file is the problem.
func existingServerNames(cwd string, names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	r := config.NewFileResolver()
	layers, err := r.Layers(context.Background(), cwd)
	if err != nil {
		return out
	}
	for _, l := range layers {
		if l.Source != config.SourceUser {
			continue
		}
		for _, name := range names {
			if _, ok := l.Settings.MCPServers[name]; ok {
				out[name] = true
			}
		}
	}
	return out
}
