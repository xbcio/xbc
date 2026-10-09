package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
)

// --- parseArgs --------------------------------------------------------

// TestParseArgsNoArguments pins the zero-value case: nothing on the command
// line must decode to a command whose every field is the zero value,
// including profile falling back to an empty XBC_PROFILE rather than whatever
// happens to be set in the shell the test suite runs under.
func TestParseArgsNoArguments(t *testing.T) {
	t.Setenv("XBC_PROFILE", "")

	cmd, err := parseArgs(nil, "XBC_", nil)
	require.NoError(t, err)
	assert.Equal(t, command{}, cmd, "command should get zero value when no parameters")
}

// TestParseArgsConfigFlag pins --config.
func TestParseArgsConfigFlag(t *testing.T) {
	cmd, err := parseArgs([]string{"--config", "/tmp/command-x.yaml"}, "XBC_", nil)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/command-x.yaml", cmd.config)
}

// TestParseArgsProfileFlagOverridesEnv pins that an explicit --profile wins
// over XBC_PROFILE -- the flag is what an operator typed for this run, the
// env var is ambient and easy to forget is set.
func TestParseArgsProfileFlagOverridesEnv(t *testing.T) {
	t.Setenv("XBC_PROFILE", "staging")

	cmd, err := parseArgs([]string{"--profile", "prod"}, "XBC_", nil)
	require.NoError(t, err)
	assert.Equal(t, "prod", cmd.profile, "Explicit --profile must override environment variable")
}

// TestParseArgsProfileFallsBackToEnv pins the other half: with no --profile
// flag at all, XBC_PROFILE is read.
func TestParseArgsProfileFallsBackToEnv(t *testing.T) {
	t.Setenv("XBC_PROFILE", "staging")

	cmd, err := parseArgs(nil, "XBC_", nil)
	require.NoError(t, err)
	assert.Equal(t, "staging", cmd.profile, "Must fallback to read XBC_PROFILE when --profile is not passed")
}

// TestParseArgsProfileFallbackFollowsTheEnvPrefix pins that the profile
// fallback is read relative to the caller's prefix rather than a hardcoded
// XBC_. The configuration layer reserves the profile variable relative to the
// same prefix, so a hardcoded name here would silently disagree with it: an
// embedder using MYAPP_ would find MYAPP_PROFILE reserved but never read, and
// XBC_PROFILE read but not reserved.
func TestParseArgsProfileFallbackFollowsTheEnvPrefix(t *testing.T) {
	t.Setenv("XBC_PROFILE", "wrong")
	t.Setenv("MYAPP_PROFILE", "staging")

	cmd, err := parseArgs(nil, "MYAPP_", nil)
	require.NoError(t, err)
	assert.Equal(t, "staging", cmd.profile, "The fallback must follow the supplied prefix, not a hardcoded one")
}

// TestParseArgsMigrateFlag pins --migrate as a bare boolean flag, distinct
// from the "migrate" subcommand.
func TestParseArgsMigrateFlag(t *testing.T) {
	cmd, err := parseArgs([]string{"--migrate"}, "XBC_", nil)
	require.NoError(t, err)
	assert.True(t, cmd.migrate)
	assert.Empty(t, cmd.subcommand, "--migrate is a flag, should not be treated as a subcommand")
}

// TestParseArgsDoctorSubcommand pins the "doctor" subcommand.
func TestParseArgsDoctorSubcommand(t *testing.T) {
	cmd, err := parseArgs([]string{"doctor"}, "XBC_", nil)
	require.NoError(t, err)
	assert.Equal(t, "doctor", cmd.subcommand)
}

// TestParseArgsValidateSubcommand pins the "validate" subcommand. It is the
// constructing, non-serving command: it runs every Preflight hook and unwinds,
// so parsing it must not be confused with either a bare run or a migration.
func TestParseArgsValidateSubcommand(t *testing.T) {
	cmd, err := parseArgs([]string{"validate", "--config", "/tmp/command-z.yaml"}, "XBC_", nil)
	require.NoError(t, err)
	assert.Equal(t, "validate", cmd.subcommand)
	assert.False(t, cmd.migrate, "the validate subcommand must not imply --migrate")
	assert.Equal(t, "/tmp/command-z.yaml", cmd.config, "Flags after subcommand must still be parsed")
}

// TestParseArgsMigrateSubcommand pins the "migrate" subcommand, and that
// flags following it are still parsed -- parseArgs peels the subcommand off
// as the first non-flag argument and hands the rest to flag.FlagSet.Parse.
func TestParseArgsMigrateSubcommand(t *testing.T) {
	cmd, err := parseArgs([]string{"migrate", "--config", "/tmp/command-y.yaml"}, "XBC_", nil)
	require.NoError(t, err)
	assert.Equal(t, "migrate", cmd.subcommand)
	assert.Equal(t, "/tmp/command-y.yaml", cmd.config, "Flags after subcommand must still be parsed")
}

// TestParseArgsIllegalFlagReturnsError pins that an undeclared flag is
// rejected by the flag package itself, not silently ignored.
func TestParseArgsIllegalFlagReturnsError(t *testing.T) {
	_, err := parseArgs([]string{"--this-flag-does-not-exist"}, "XBC_", nil)
	require.Error(t, err)
}

// TestParseArgsUnknownSubcommandReturnsError pins that a first non-flag
// argument other than "migrate"/"doctor" is rejected with a message naming
// the two legal subcommands, rather than being treated as some other kind of
// argument.
func TestParseArgsUnknownSubcommandReturnsError(t *testing.T) {
	_, err := parseArgs([]string{"frobnicate"}, "XBC_", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown subcommand")
}

// TestParseArgsRejectsTrailingPositionalArgs pins that leftover positional
// arguments after flag parsing are an error, not silently dropped -- a typo'd
// flag value that flag.Parse happens to treat as positional must still
// surface.
func TestParseArgsRejectsTrailingPositionalArgs(t *testing.T) {
	_, err := parseArgs([]string{"--config", "/tmp/command-z.yaml", "extra"}, "XBC_", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown argument")
}

// --- consumer-defined subcommands ---------------------------------------

// consumerCommands is the registration the parsing cases below select by name.
// The bodies do nothing: parsing decides which command was asked for and what
// it is handed, and execution is tested through Execute.
func consumerCommands() []customCommand {
	noop := func(context.Context, *config.Environment, []string) error { return nil }
	return []customCommand{
		{name: "cleanup", usage: "remove expired rows", run: noop},
		{name: "report", usage: "print a report", run: noop},
	}
}

// TestParseArgsConsumerSubcommandOwnsEverythingAfterItsName pins the split
// that makes a command's flag namespace its own: the runtime parses what
// precedes the name, and everything after it reaches the command verbatim,
// including arguments spelled like the shared flags.
func TestParseArgsConsumerSubcommandOwnsEverythingAfterItsName(t *testing.T) {
	cmd, err := parseArgs([]string{"cleanup", "--dry-run", "-config", "theirs.yml"}, "XBC_", consumerCommands())
	require.NoError(t, err)
	assert.Equal(t, "cleanup", cmd.subcommand)
	assert.Equal(t, []string{"--dry-run", "-config", "theirs.yml"}, cmd.args,
		"everything after the command name belongs to the command")
	assert.Empty(t, cmd.config, "a shared flag after the name is not the runtime's")
	assert.False(t, cmd.migrate)
}

// TestParseArgsSharedFlagsPrecedeAConsumerSubcommand pins the other half of
// that split: --config and --profile are read where they precede the name, in
// both the separate-value and the "=" spelling, so a command can be run
// against a chosen configuration and profile.
func TestParseArgsSharedFlagsPrecedeAConsumerSubcommand(t *testing.T) {
	cmd, err := parseArgs(
		[]string{"--config", "/tmp/command-split.yml", "--profile", "prod", "cleanup", "x"},
		"XBC_", consumerCommands())
	require.NoError(t, err)
	assert.Equal(t, "cleanup", cmd.subcommand)
	assert.Equal(t, "/tmp/command-split.yml", cmd.config)
	assert.Equal(t, "prod", cmd.profile)
	assert.Equal(t, []string{"x"}, cmd.args)

	cmd, err = parseArgs([]string{"--config=/tmp/command-eq.yml", "cleanup"}, "XBC_", consumerCommands())
	require.NoError(t, err)
	assert.Equal(t, "/tmp/command-eq.yml", cmd.config,
		"an = spelling carries its own value and cannot swallow the subcommand token")
	assert.Equal(t, "cleanup", cmd.subcommand)
}

// TestParseArgsRejectsMigrateWithAConsumerSubcommand pins the one combination
// of shared flags and a command that is refused: migration belongs to a boot,
// and a command that wanted one would have to say which schema it is migrating
// -- a decision the framework does not make for it.
func TestParseArgsRejectsMigrateWithAConsumerSubcommand(t *testing.T) {
	cmd, err := parseArgs([]string{"--migrate", "cleanup"}, "XBC_", consumerCommands())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `--migrate cannot be combined with the "cleanup" command`)
	assert.Empty(t, cmd.subcommand, "a refused line selects no command")
}

// TestParseArgsUnknownSubcommandListsApplicationCommands pins the message an
// unrecognized first argument produces. With nothing registered it must stay
// the message this runtime has always printed, word for word; with commands
// registered it names them beside the built-ins, in registration order.
func TestParseArgsUnknownSubcommandListsApplicationCommands(t *testing.T) {
	_, err := parseArgs([]string{"frobnicate"}, "XBC_", nil)
	require.EqualError(t, err,
		`xbc: unknown subcommand "frobnicate"; use migrate, doctor, validate, or no subcommand to start`)

	_, err = parseArgs([]string{"frobnicate"}, "XBC_", consumerCommands())
	require.EqualError(t, err,
		`xbc: unknown subcommand "frobnicate"; use migrate, doctor, validate, cleanup, report, or no subcommand to start`)
}

// TestParseArgsBuiltinSubcommandsAreStillFoundAfterFlags pins the grammar the
// shared flags and the subcommand share: the token is the first positional
// argument, so it may follow the flags. That was an error before custom
// commands existed and is the spelling a command needs, and a built-in reads
// the same either way.
func TestParseArgsBuiltinSubcommandsAreStillFoundAfterFlags(t *testing.T) {
	cmd, err := parseArgs([]string{"--config", "/tmp/command-order.yml", "doctor"}, "XBC_", nil)
	require.NoError(t, err)
	assert.Equal(t, "doctor", cmd.subcommand)
	assert.Equal(t, "/tmp/command-order.yml", cmd.config)
}

// TestParseArgsPositionalAfterFlagsStaysAnUnknownArgument pins the boundary of
// the token rule: an unrecognized positional after flags keeps the reading it
// had before custom commands -- a stray argument, reported as one -- rather
// than being promoted to an unknown subcommand.
func TestParseArgsPositionalAfterFlagsStaysAnUnknownArgument(t *testing.T) {
	_, err := parseArgs([]string{"--config", "/tmp/command-z.yaml", "extra"}, "XBC_", consumerCommands())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown argument [extra]")
}

// TestWithCommandRejectionsNameTheirOption pins that every registration a
// framework would have to guess about is refused where the caller wrote it: an
// empty name, a name that can only be read as a flag, a reserved name, a
// missing body, and a duplicate. The error is the ordinary option error, so it
// names the option index like every other refused Option.
func TestWithCommandRejectionsNameTheirOption(t *testing.T) {
	noop := func(context.Context, *config.Environment, []string) error { return nil }
	cases := []struct {
		name    string
		option  Option
		wantErr string
	}{
		{
			name:    "reserved name",
			option:  WithCommand("doctor", "diagnose the process", noop),
			wantErr: `xbc: option 0 registers subcommand "doctor", which the runtime already owns`,
		},
		{
			name:    "empty name",
			option:  WithCommand("", "do something", noop),
			wantErr: "xbc: option 0 registers a subcommand with an empty name",
		},
		{
			name:    "flag-shaped name",
			option:  WithCommand("-cleanup", "remove expired rows", noop),
			wantErr: `xbc: option 0 registers subcommand "-cleanup", and a name starting with "-" can only be read as a flag`,
		},
		{
			name:    "missing body",
			option:  WithCommand("cleanup", "remove expired rows", nil),
			wantErr: `xbc: option 0 registers subcommand "cleanup" without a run function`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.option)
			require.EqualError(t, err, tc.wantErr)
		})
	}

	_, err := New(
		WithCommand("cleanup", "remove expired rows", noop),
		WithCommand("cleanup", "remove expired rows", noop),
	)
	require.EqualError(t, err, `xbc: option 1 registers subcommand "cleanup" twice`)
}

// TestWithCommandKeepsRegistrationOrder pins that the registered commands reach
// the App in the order the caller wrote them: the usage listing and the
// unknown-subcommand message both enumerate them, and an order that followed a
// map would shuffle between runs.
func TestWithCommandKeepsRegistrationOrder(t *testing.T) {
	noop := func(context.Context, *config.Environment, []string) error { return nil }
	app, err := New(
		WithBundles(),
		WithCommand("report", "print a report", noop),
		WithCommand("cleanup", "remove expired rows", noop),
	)
	require.NoError(t, err)
	require.Len(t, app.commands, 2)
	assert.Equal(t, "report", app.commands[0].name)
	assert.Equal(t, "cleanup", app.commands[1].name)
}

// --- wantsMigration -----------------------------------------------------

// TestWantsMigrationTruthTable pins the full truth table described in
// wantsMigration's own doc comment: the three sources are OR'd, none of them
// can veto another, and false requires every source to be false.
//
// The third source is the deployment's xbc.auto_migrate setting, which
// reaches this decision as a bare bool from the runtime settings.
func TestWantsMigrationTruthTable(t *testing.T) {
	cases := []struct {
		name        string
		cmd         command
		autoMigrate bool
		want        bool
	}{
		{"Only --migrate is true", command{migrate: true}, false, true},
		{"Only migrate subcommand is true", command{subcommand: "migrate"}, false, true},
		{"Only auto_migrate is true", command{}, true, true},
		{"--migrate and auto_migrate both being true is still true", command{migrate: true}, true, true},
		{"doctor subcommand itself does not trigger migration", command{subcommand: "doctor"}, false, false},
		{"validate subcommand itself does not trigger migration", command{subcommand: "validate"}, false, false},
		// wantsMigration answers only whether migration was asked for; the
		// validate command is the branch above the answer that never reads it.
		// TestValidateNeverMigrates pins that ordering.
		{"auto_migrate still asks under validate", command{subcommand: "validate"}, true, true},
		{"False when all three are false", command{}, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cmd.wantsMigration(tc.autoMigrate)
			assert.Equal(t, tc.want, got,
				"wantsMigration must be logical OR of --migrate, migrate subcommand, and xbc.auto_migrate")
		})
	}
}
