package runtime

import (
	"flag"
	"fmt"
	"os"
)

// command is the runtime's parsed process command: an optional subcommand plus
// the flags shared by every execution mode. It stays private because XBC does
// not expose command parsing as a reusable framework capability.
type command struct {
	subcommand string // "" (normal boot) / "migrate" / "doctor"
	config     string
	profile    string
	migrate    bool
}

// parseArgs parses the small command surface owned by the runtime process
// adapter. The subcommand, if present, must be the first non-flag argument;
// flag requires every flag to precede positional arguments, so it is peeled
// off before handing the rest to fs.Parse.
//
// envPrefix is the configuration environment prefix. The profile fallback is
// read relative to it rather than from a hardcoded "XBC_", so command parsing
// and configuration loading cannot disagree when an embedder chooses another
// prefix.
func parseArgs(args []string, envPrefix string) (command, error) {
	var cmd command
	fs := flag.NewFlagSet("xbc", flag.ContinueOnError)
	fs.StringVar(&cmd.config, "config", "", "configuration file path")
	fs.StringVar(&cmd.profile, "profile", "", "configuration profile (read <prefix>PROFILE if not set)")
	fs.BoolVar(&cmd.migrate, "migrate", false, "run migration once before starting")

	rest := args
	if len(rest) > 0 && rest[0] != "" && rest[0][0] != '-' {
		switch rest[0] {
		case "migrate", "doctor":
			cmd.subcommand = rest[0]
			rest = rest[1:]
		default:
			fs.Usage()
			return cmd, fmt.Errorf("xbc: unknown subcommand %q; use migrate, doctor, or no subcommand to start", rest[0])
		}
	}

	if err := fs.Parse(rest); err != nil {
		return cmd, err // the flag package has already printed usage to fs.Output() (os.Stderr by default)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return cmd, fmt.Errorf("xbc: unknown argument %v", fs.Args())
	}

	if cmd.profile == "" {
		cmd.profile = os.Getenv(envPrefix + "PROFILE")
	}
	return cmd, nil
}

// wantsMigration folds the three independent migration requests into one
// answer. They are OR'd because the command flag, subcommand, and deployment
// setting are separate positive authorities; none represents an explicit veto.
func (c command) wantsMigration(autoMigrate bool) bool {
	return c.migrate || c.subcommand == "migrate" || autoMigrate
}
