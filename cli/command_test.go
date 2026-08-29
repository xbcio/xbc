package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- ParseArgs --------------------------------------------------------

// TestParseArgsNoArguments pins the zero-value case: nothing on the command
// line must decode to a Command whose every field is the zero value,
// including Profile falling back to an empty XBC_PROFILE rather than whatever
// happens to be set in the shell the test suite runs under.
func TestParseArgsNoArguments(t *testing.T) {
	t.Setenv("XBC_PROFILE", "")

	cmd, err := ParseArgs(nil)
	require.NoError(t, err)
	assert.Equal(t, Command{}, cmd, "无参数时应得到全零值的 Command")
}

// TestParseArgsConfigFlag pins --config.
func TestParseArgsConfigFlag(t *testing.T) {
	cmd, err := ParseArgs([]string{"--config", "/tmp/command-x.yaml"})
	require.NoError(t, err)
	assert.Equal(t, "/tmp/command-x.yaml", cmd.Config)
}

// TestParseArgsProfileFlagOverridesEnv pins that an explicit --profile wins
// over XBC_PROFILE -- the flag is what an operator typed for this run, the
// env var is ambient and easy to forget is set.
func TestParseArgsProfileFlagOverridesEnv(t *testing.T) {
	t.Setenv("XBC_PROFILE", "staging")

	cmd, err := ParseArgs([]string{"--profile", "prod"})
	require.NoError(t, err)
	assert.Equal(t, "prod", cmd.Profile, "显式 --profile 必须覆盖环境变量")
}

// TestParseArgsProfileFallsBackToEnv pins the other half: with no --profile
// flag at all, XBC_PROFILE is read.
func TestParseArgsProfileFallsBackToEnv(t *testing.T) {
	t.Setenv("XBC_PROFILE", "staging")

	cmd, err := ParseArgs(nil)
	require.NoError(t, err)
	assert.Equal(t, "staging", cmd.Profile, "未传 --profile 时必须回退读取 XBC_PROFILE")
}

// TestParseArgsMigrateFlag pins --migrate as a bare boolean flag, distinct
// from the "migrate" subcommand.
func TestParseArgsMigrateFlag(t *testing.T) {
	cmd, err := ParseArgs([]string{"--migrate"})
	require.NoError(t, err)
	assert.True(t, cmd.Migrate)
	assert.Empty(t, cmd.Subcommand, "--migrate 是一个标志，不应被当成子命令")
}

// TestParseArgsDoctorSubcommand pins the "doctor" subcommand.
func TestParseArgsDoctorSubcommand(t *testing.T) {
	cmd, err := ParseArgs([]string{"doctor"})
	require.NoError(t, err)
	assert.Equal(t, "doctor", cmd.Subcommand)
}

// TestParseArgsMigrateSubcommand pins the "migrate" subcommand, and that
// flags following it are still parsed -- ParseArgs peels the subcommand off
// as the first non-flag argument and hands the rest to flag.FlagSet.Parse.
func TestParseArgsMigrateSubcommand(t *testing.T) {
	cmd, err := ParseArgs([]string{"migrate", "--config", "/tmp/command-y.yaml"})
	require.NoError(t, err)
	assert.Equal(t, "migrate", cmd.Subcommand)
	assert.Equal(t, "/tmp/command-y.yaml", cmd.Config, "子命令之后的 flag 仍必须被解析")
}

// TestParseArgsIllegalFlagReturnsError pins that an undeclared flag is
// rejected by the flag package itself, not silently ignored.
func TestParseArgsIllegalFlagReturnsError(t *testing.T) {
	_, err := ParseArgs([]string{"--this-flag-does-not-exist"})
	require.Error(t, err)
}

// TestParseArgsUnknownSubcommandReturnsError pins that a first non-flag
// argument other than "migrate"/"doctor" is rejected with a message naming
// the two legal subcommands, rather than being treated as some other kind of
// argument.
func TestParseArgsUnknownSubcommandReturnsError(t *testing.T) {
	_, err := ParseArgs([]string{"frobnicate"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未知子命令")
}

// TestParseArgsRejectsTrailingPositionalArgs pins that leftover positional
// arguments after flag parsing are an error, not silently dropped -- a typo'd
// flag value that flag.Parse happens to treat as positional must still
// surface.
func TestParseArgsRejectsTrailingPositionalArgs(t *testing.T) {
	_, err := ParseArgs([]string{"--config", "/tmp/command-z.yaml", "extra"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未知参数")
}

// --- WantsMigration -----------------------------------------------------

// TestWantsMigrationTruthTable pins the full truth table described in
// WantsMigration's own doc comment: the three sources are OR'd, none of them
// can veto another, and false requires every source to be false.
//
// The third source is the deployment's xbc.auto_migrate setting, which
// reaches this package as a bare bool -- package runtime owns the private
// settings value it is read from.
func TestWantsMigrationTruthTable(t *testing.T) {
	cases := []struct {
		name        string
		cmd         Command
		autoMigrate bool
		want        bool
	}{
		{"仅 --migrate 为真", Command{Migrate: true}, false, true},
		{"仅 migrate 子命令为真", Command{Subcommand: "migrate"}, false, true},
		{"仅 auto_migrate 为真", Command{}, true, true},
		{"--migrate 与 auto_migrate 同时为真仍然是真", Command{Migrate: true}, true, true},
		{"doctor 子命令本身不触发迁移", Command{Subcommand: "doctor"}, false, false},
		{"三者都为假时为假", Command{}, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cmd.WantsMigration(tc.autoMigrate)
			assert.Equal(t, tc.want, got,
				"WantsMigration 必须是 --migrate、migrate 子命令、xbc.auto_migrate 三者的逻辑或")
		})
	}
}
