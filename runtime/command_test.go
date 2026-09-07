package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- parseArgs --------------------------------------------------------

// TestParseArgsNoArguments pins the zero-value case: nothing on the command
// line must decode to a command whose every field is the zero value,
// including profile falling back to an empty XBC_PROFILE rather than whatever
// happens to be set in the shell the test suite runs under.
func TestParseArgsNoArguments(t *testing.T) {
	t.Setenv("XBC_PROFILE", "")

	cmd, err := parseArgs(nil, "XBC_")
	require.NoError(t, err)
	assert.Equal(t, command{}, cmd, "command should get zero value when no parameters")
}

// TestParseArgsConfigFlag pins --config.
func TestParseArgsConfigFlag(t *testing.T) {
	cmd, err := parseArgs([]string{"--config", "/tmp/command-x.yaml"}, "XBC_")
	require.NoError(t, err)
	assert.Equal(t, "/tmp/command-x.yaml", cmd.config)
}

// TestParseArgsProfileFlagOverridesEnv pins that an explicit --profile wins
// over XBC_PROFILE -- the flag is what an operator typed for this run, the
// env var is ambient and easy to forget is set.
func TestParseArgsProfileFlagOverridesEnv(t *testing.T) {
	t.Setenv("XBC_PROFILE", "staging")

	cmd, err := parseArgs([]string{"--profile", "prod"}, "XBC_")
	require.NoError(t, err)
	assert.Equal(t, "prod", cmd.profile, "Explicit --profile must override environment variable")
}

// TestParseArgsProfileFallsBackToEnv pins the other half: with no --profile
// flag at all, XBC_PROFILE is read.
func TestParseArgsProfileFallsBackToEnv(t *testing.T) {
	t.Setenv("XBC_PROFILE", "staging")

	cmd, err := parseArgs(nil, "XBC_")
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

	cmd, err := parseArgs(nil, "MYAPP_")
	require.NoError(t, err)
	assert.Equal(t, "staging", cmd.profile, "The fallback must follow the supplied prefix, not a hardcoded one")
}

// TestParseArgsMigrateFlag pins --migrate as a bare boolean flag, distinct
// from the "migrate" subcommand.
func TestParseArgsMigrateFlag(t *testing.T) {
	cmd, err := parseArgs([]string{"--migrate"}, "XBC_")
	require.NoError(t, err)
	assert.True(t, cmd.migrate)
	assert.Empty(t, cmd.subcommand, "--migrate is a flag, should not be treated as a subcommand")
}

// TestParseArgsDoctorSubcommand pins the "doctor" subcommand.
func TestParseArgsDoctorSubcommand(t *testing.T) {
	cmd, err := parseArgs([]string{"doctor"}, "XBC_")
	require.NoError(t, err)
	assert.Equal(t, "doctor", cmd.subcommand)
}

// TestParseArgsMigrateSubcommand pins the "migrate" subcommand, and that
// flags following it are still parsed -- parseArgs peels the subcommand off
// as the first non-flag argument and hands the rest to flag.FlagSet.Parse.
func TestParseArgsMigrateSubcommand(t *testing.T) {
	cmd, err := parseArgs([]string{"migrate", "--config", "/tmp/command-y.yaml"}, "XBC_")
	require.NoError(t, err)
	assert.Equal(t, "migrate", cmd.subcommand)
	assert.Equal(t, "/tmp/command-y.yaml", cmd.config, "Flags after subcommand must still be parsed")
}

// TestParseArgsIllegalFlagReturnsError pins that an undeclared flag is
// rejected by the flag package itself, not silently ignored.
func TestParseArgsIllegalFlagReturnsError(t *testing.T) {
	_, err := parseArgs([]string{"--this-flag-does-not-exist"}, "XBC_")
	require.Error(t, err)
}

// TestParseArgsUnknownSubcommandReturnsError pins that a first non-flag
// argument other than "migrate"/"doctor" is rejected with a message naming
// the two legal subcommands, rather than being treated as some other kind of
// argument.
func TestParseArgsUnknownSubcommandReturnsError(t *testing.T) {
	_, err := parseArgs([]string{"frobnicate"}, "XBC_")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown subcommand")
}

// TestParseArgsRejectsTrailingPositionalArgs pins that leftover positional
// arguments after flag parsing are an error, not silently dropped -- a typo'd
// flag value that flag.Parse happens to treat as positional must still
// surface.
func TestParseArgsRejectsTrailingPositionalArgs(t *testing.T) {
	_, err := parseArgs([]string{"--config", "/tmp/command-z.yaml", "extra"}, "XBC_")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown argument")
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
