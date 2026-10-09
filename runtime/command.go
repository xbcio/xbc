package runtime

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/xbcio/xbc/config"
)

// The subcommands the runtime itself owns. Their names are reserved: an
// application cannot register one of them, and every error message that lists
// what a command line accepts lists them first.
//
// migrateSubcommand is spelled here rather than beside doctorSubcommand and
// validateSubcommand because more than one file asks about it -- parsing,
// migration's wantsMigration, and the dispatch in execute.
const migrateSubcommand = "migrate"

// validateSubcommand names the constructing, non-serving check: it runs every
// Preflight hook -- the assembly a Start would perform, without activating
// anything -- and unwinds. It never migrates and never listens.
const validateSubcommand = "validate"

// builtinSubcommands is the reserved set in the order the command surface has
// always listed it.
var builtinSubcommands = []string{migrateSubcommand, doctorSubcommand, validateSubcommand}

// command is the runtime's parsed process command: an optional subcommand plus
// the flags shared by every execution mode. It stays private because XBC does
// not expose command parsing as a reusable framework capability.
type command struct {
	subcommand string // "" (normal boot) / a built-in / a WithCommand registration
	config     string
	profile    string
	migrate    bool
	// args is the verbatim tail of a consumer-defined subcommand. It is nil for
	// a built-in, which takes no arguments.
	args []string
}

// CommandRun is the body of a consumer-defined subcommand registered with
// WithCommand. It receives the command line's own arguments exactly as they
// were written, the merged configuration, and a context that is canceled when
// the process is asked to stop.
type CommandRun func(ctx context.Context, env *config.Environment, args []string) error

// customCommand is one WithCommand registration.
type customCommand struct {
	name  string
	usage string
	run   CommandRun
}

// WithCommand registers a consumer-defined subcommand: "xbc <name> [args...]"
// runs run instead of booting an application.
//
// The command runs after configuration has been loaded and logging installed,
// and before anything else a boot does. It sees the merged configuration --
// every layer, with the same strict decoding a plugin's input gets -- through
// env, and the process logger through log.L(), and it can rely on neither
// placement nor construction having happened: no workload was placed, no
// factory ran, no migration ran, no listener was opened. A command that needs
// a database or a broker opens its own short-lived connection and closes it;
// the framework does not hand it a service locator.
//
// flags shared with a boot (--config, --profile) must precede the command's
// name, and everything after the name is read by the command: its flag
// namespace is its own, so "--config" after the name is an argument of the
// command rather than the runtime's flag. --migrate cannot be combined with a
// command -- migration is something a boot does, not something a subcommand
// asks for.
//
// A failing command exits 1 with the error it returned; exit codes are not
// configurable. The zero-value CommandRun and a name that is empty, reserved
// (migrate, doctor, validate), or already registered are rejected by New.
func WithCommand(name, usage string, run CommandRun) Option {
	return func(options *appOptions) error {
		options.commands = append(options.commands, customCommand{name: name, usage: usage, run: run})
		return nil
	}
}

// validateCommand checks one registration against the commands already
// registered. New calls it with the option's index, so the error names the
// option the way every other option error does.
func validateCommand(command customCommand, registered []customCommand) string {
	switch {
	case command.name == "":
		return "registers a subcommand with an empty name"
	case strings.HasPrefix(command.name, "-"):
		return fmt.Sprintf("registers subcommand %q, and a name starting with %q can only be read as a flag", command.name, "-")
	case command.run == nil:
		return fmt.Sprintf("registers subcommand %q without a run function", command.name)
	}
	for _, reserved := range builtinSubcommands {
		if command.name == reserved {
			return fmt.Sprintf("registers subcommand %q, which the runtime already owns", command.name)
		}
	}
	for _, existing := range registered {
		if existing.name == command.name {
			return fmt.Sprintf("registers subcommand %q twice", command.name)
		}
	}
	return ""
}

// customCommand returns the consumer-defined command named name, and whether
// one is registered under it.
func (a *App) customCommand(name string) (customCommand, bool) {
	for _, command := range a.commands {
		if command.name == name {
			return command, true
		}
	}
	return customCommand{}, false
}

// parseArgs parses the command surface owned by the runtime process adapter:
// the shared flags, an optional subcommand, and a consumer-defined
// subcommand's own arguments.
//
// The subcommand is the first positional argument, so the shared flags may
// precede it. What follows a built-in subcommand is parsed as flags, which is
// how "xbc doctor --config application.yml" has always been written; what
// follows a consumer-defined subcommand belongs to that command verbatim,
// because its flag namespace is its own. An unrecognized positional stays what
// it was before custom commands existed: "xbc frobnicate" is an unknown
// subcommand, a positional after flags is an unknown argument.
//
// envPrefix is the configuration environment prefix. The profile fallback is
// read relative to it rather than from a hardcoded "XBC_", so command parsing
// and configuration loading cannot disagree when an embedder chooses another
// prefix.
func parseArgs(args []string, envPrefix string, commands []customCommand) (command, error) {
	var cmd command
	fs := flag.NewFlagSet("xbc", flag.ContinueOnError)
	fs.StringVar(&cmd.config, "config", "", "configuration file path")
	fs.StringVar(&cmd.profile, "profile", "", "configuration profile (read <prefix>PROFILE if not set)")
	fs.BoolVar(&cmd.migrate, "migrate", false, "run migration once before starting")
	fs.Usage = func() { printCommandUsage(fs, commands) }

	switch token := subcommandToken(args); {
	case token < 0:
		if err := fs.Parse(args); err != nil {
			return cmd, err // the flag package has already printed usage to fs.Output() (os.Stderr by default)
		}
	case isBuiltinSubcommand(args[token]):
		cmd.subcommand = args[token]
		if err := fs.Parse(withoutArgument(args, token)); err != nil {
			return cmd, err
		}
	default:
		registered, ok := lookupCommand(commands, args[token])
		switch {
		case ok:
			// Only what precedes the token is the runtime's; the command owns
			// the rest, including anything that looks like a shared flag.
			if err := fs.Parse(args[:token]); err != nil {
				return cmd, err
			}
			if cmd.migrate {
				fs.Usage()
				return cmd, fmt.Errorf("xbc: --migrate cannot be combined with the %q command; run it separately from a migration", registered.name)
			}
			cmd.subcommand = registered.name
			cmd.args = append([]string(nil), args[token+1:]...)
		case token == 0:
			// The first argument is the one position where an unrecognized
			// token has always been an unknown subcommand rather than a stray
			// argument, and where the message can name what is on offer.
			fs.Usage()
			return cmd, fmt.Errorf("xbc: unknown subcommand %q; use %s, or no subcommand to start", args[0], subcommandList(commands))
		default:
			if err := fs.Parse(args); err != nil {
				return cmd, err
			}
		}
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

// subcommandToken returns the index of the argument that can only be the
// subcommand -- the first positional argument -- or -1 when the line holds
// none. It cannot simply scan for the first argument without a leading "-":
// the two value flags would then hand their values back as the token, and the
// flag package's rules for what consumes the next argument are what make
// "xbc --config cleanup" a configuration path and not a subcommand.
func subcommandToken(args []string) int {
	for index := 0; index < len(args); index++ {
		switch arg := args[index]; {
		case arg == "" || arg == "--":
			// An empty argument is not a token -- the peel has always required
			// a non-empty first argument, and one here stays the stray
			// positional it was. "--" ends flag parsing without being an
			// argument; what follows it is still positional.
			continue
		case arg[0] != '-':
			return index
		case consumesNextArgument(arg):
			index++
		}
	}
	return -1
}

// consumesNextArgument reports whether a flag written as arg takes its value
// from the next argument rather than from an "=" suffix. It is the one place
// the shared flags' arity is written down besides their declarations.
func consumesNextArgument(arg string) bool {
	name := strings.TrimLeft(arg, "-")
	if strings.ContainsRune(name, '=') {
		return false
	}
	return name == "config" || name == "profile"
}

// withoutArgument returns args with the argument at index removed. A built-in
// subcommand keeps the parsing the flag package has always done over the rest
// of the line; the token is taken out first because it is not a flag and would
// otherwise stop flag parsing at itself.
func withoutArgument(args []string, index int) []string {
	rest := make([]string, 0, len(args)-1)
	rest = append(rest, args[:index]...)
	return append(rest, args[index+1:]...)
}

// lookupCommand finds the consumer-defined command named name.
func lookupCommand(commands []customCommand, name string) (customCommand, bool) {
	for _, command := range commands {
		if command.name == name {
			return command, true
		}
	}
	return customCommand{}, false
}

// isBuiltinSubcommand reports whether name is one of the subcommands the
// runtime owns.
func isBuiltinSubcommand(name string) bool {
	for _, builtin := range builtinSubcommands {
		if name == builtin {
			return true
		}
	}
	return false
}

// subcommandList names every subcommand a command line accepts, built-ins
// first, for the message that rejects an unknown one.
func subcommandList(commands []customCommand) string {
	names := append([]string(nil), builtinSubcommands...)
	for _, command := range commands {
		names = append(names, command.name)
	}
	return strings.Join(names, ", ")
}

// printCommandUsage writes the flag set's own usage and, when the application
// registered any, the subcommands it added. With no registration this is byte
// for byte what the flag package prints on its own, so an application that
// declares no command does not see its usage text change.
func printCommandUsage(fs *flag.FlagSet, commands []customCommand) {
	fmt.Fprintf(fs.Output(), "Usage of %s:\n", fs.Name())
	fs.PrintDefaults()
	if len(commands) == 0 {
		return
	}
	fmt.Fprint(fs.Output(), "\nApplication commands:\n")
	for _, command := range commands {
		fmt.Fprintf(fs.Output(), "  %s\t%s\n", command.name, command.usage)
	}
}

// wantsMigration folds the three independent migration requests into one
// answer. They are OR'd because the command flag, subcommand, and deployment
// setting are separate positive authorities; none represents an explicit veto.
func (c command) wantsMigration(autoMigrate bool) bool {
	return c.migrate || c.subcommand == migrateSubcommand || autoMigrate
}
