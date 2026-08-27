// Package cli parses the framework's command line: an optional subcommand
// plus the flags every subcommand shares.
//
// It is deliberately the whole command-line domain and nothing else. It reads
// os.Getenv for the profile fallback and returns a value; it does not load
// configuration, does not know what a plugin is, and does not know the root
// package exists. That one-way direction lets the root package hold the
// assembled Command as plain data and decide what to do with it.
//
// Per the package-layout design's import direction rules (§6), it depends on
// nothing but the standard library, and must not import the root package,
// internal/container, the runtime stack, gin or grpc.
package cli

import (
	"flag"
	"fmt"
	"os"
)

// Command is the parsed command line: an optional subcommand plus the flags
// every subcommand shares.
type Command struct {
	Subcommand string // "" (normal boot) / "migrate" / "doctor"
	Config     string
	Profile    string
	Migrate    bool
}

// ParseArgs parses args with the standard library flag package -- the design
// deliberately excludes cobra, since xbc only ever has two subcommands and a
// handful of flags. The subcommand, if present, must be the first non-flag
// argument; flag requires every flag to precede positional arguments, so it
// is peeled off before handing the rest to fs.Parse.
func ParseArgs(args []string) (Command, error) {
	var cmd Command
	fs := flag.NewFlagSet("xbc", flag.ContinueOnError)
	fs.StringVar(&cmd.Config, "config", "", "配置文件路径")
	fs.StringVar(&cmd.Profile, "profile", "", "配置 profile（未设置时读取 XBC_PROFILE）")
	fs.BoolVar(&cmd.Migrate, "migrate", false, "启动前先跑一次迁移")

	rest := args
	if len(rest) > 0 && rest[0] != "" && rest[0][0] != '-' {
		switch rest[0] {
		case "migrate", "doctor":
			cmd.Subcommand = rest[0]
			rest = rest[1:]
		default:
			fs.Usage()
			return cmd, fmt.Errorf("xbc: 未知子命令 %q，可选 migrate/doctor，或不带子命令直接启动", rest[0])
		}
	}

	if err := fs.Parse(rest); err != nil {
		return cmd, err // the flag package has already printed usage to fs.Output() (os.Stderr by default)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return cmd, fmt.Errorf("xbc: 未知参数 %v", fs.Args())
	}

	if cmd.Profile == "" {
		cmd.Profile = os.Getenv("XBC_PROFILE")
	}
	return cmd, nil
}

// WantsMigration folds the three independent ways a run can be told to
// migrate into one answer. They are deliberately OR'd rather than ranked:
// each is a separate authority (the operator's flag, the operator's
// subcommand, the deployment's config) and none of them is a way of saying
// "do not migrate", so there is nothing for a precedence rule to arbitrate.
//
// The third authority arrives as a bare bool rather than as the root
// package's private settings value, and that is the point: taking settings
// would make this package know a configuration type it otherwise has no use
// for. The deployment's vote really is just one boolean, so that is what the
// parameter says.
func (c Command) WantsMigration(autoMigrate bool) bool {
	return c.Migrate || c.Subcommand == "migrate" || autoMigrate
}
