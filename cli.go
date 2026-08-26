package xbc

import (
	"flag"
	"fmt"
	"os"
)

// cliOptions is the parsed command line: an optional subcommand plus the
// flags every subcommand shares.
type cliOptions struct {
	subcommand string // "" (normal boot) / "migrate" / "doctor"
	config     string
	profile    string
	migrate    bool
}

// parseArgs parses args with the standard library flag package -- spec §4.2
// deliberately excludes cobra, since xbc only ever has two subcommands and a
// handful of flags. The subcommand, if present, must be the first
// non-flag argument; flag requires every flag to precede positional
// arguments, so it is peeled off before handing the rest to fs.Parse.
func parseArgs(args []string) (cliOptions, error) {
	var opts cliOptions
	fs := flag.NewFlagSet("xbc", flag.ContinueOnError)
	fs.StringVar(&opts.config, "config", "", "配置文件路径")
	fs.StringVar(&opts.profile, "profile", "", "配置 profile（未设置时读取 XBC_PROFILE）")
	fs.BoolVar(&opts.migrate, "migrate", false, "启动前先跑一次迁移")

	rest := args
	if len(rest) > 0 && rest[0] != "" && rest[0][0] != '-' {
		switch rest[0] {
		case "migrate", "doctor":
			opts.subcommand = rest[0]
			rest = rest[1:]
		default:
			fs.Usage()
			return opts, fmt.Errorf("xbc: 未知子命令 %q，可选 migrate/doctor，或不带子命令直接启动", rest[0])
		}
	}

	if err := fs.Parse(rest); err != nil {
		return opts, err // the flag package has already printed usage to fs.Output() (os.Stderr by default)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return opts, fmt.Errorf("xbc: 未知参数 %v", fs.Args())
	}

	if opts.profile == "" {
		opts.profile = os.Getenv("XBC_PROFILE")
	}
	return opts, nil
}
