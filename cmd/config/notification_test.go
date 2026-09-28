package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unknwon/goconfig" //nolint:misspell // Don't include misspell when running golangci-lint
)

func TestNotificationConfigCLIHelper(t *testing.T) {
	if os.Getenv("RCLONE_NOTIFICATION_CONFIG_CLI_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"rclone"}, os.Args[i+1:]...)
			cmd.Main()
			return
		}
	}
	t.Fatal("missing rclone arguments")
}

func TestNotificationConfigCLI(t *testing.T) {
	const token = "123456:synthetic_token"
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	require.NoError(t, os.WriteFile(configPath, []byte("[ordinary]\ntype = local\ndescription = Existing remote\n"), 0o600))
	executable, err := os.Executable()
	require.NoError(t, err)
	run := func(answers ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		process := exec.CommandContext(ctx, executable, "-test.run=^TestNotificationConfigCLIHelper$", "--", "config", "--config", configPath)
		for _, value := range os.Environ() {
			name, _, _ := strings.Cut(value, "=")
			if !strings.HasPrefix(strings.ToUpper(name), "RCLONE_") {
				process.Env = append(process.Env, value)
			}
		}
		process.Env = append(process.Env, "RCLONE_NOTIFICATION_CONFIG_CLI_HELPER=1")
		process.Stdin = strings.NewReader(strings.Join(answers, "\n") + "\n")
		output, err := process.CombinedOutput()
		require.NoError(t, err, "%s", output)
		assert.NotContains(t, string(output), token)
		return string(output)
	}
	readConfig := func() *goconfig.ConfigFile {
		t.Helper()
		cfg, err := goconfig.LoadConfigFile(configPath)
		require.NoError(t, err)
		ordinary, err := cfg.GetSection("ordinary")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"type": "local", "description": "Existing remote"}, ordinary)
		return cfg
	}

	output := run("o", "n", "telegram-me", "1", token, "-12345", "Backup", "y", "q", "q")
	cfg := readConfig()
	profile, err := cfg.GetSection("notify:telegram-me")
	require.NoError(t, err)
	assert.Equal(t, "telegram", profile["provider"])
	assert.Equal(t, "-12345", profile["chat_id"])
	assert.Equal(t, "Backup", profile["label"])
	storedToken := profile["token"]
	encodedToken, ok := strings.CutPrefix(storedToken, "obscured:")
	require.True(t, ok, "saved token must be obscured")
	revealedToken, err := obscure.Reveal(encodedToken)
	require.NoError(t, err)
	assert.Equal(t, token, revealedToken)
	assert.NotContains(t, output, storedToken)
	contents, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.NotContains(t, string(contents), token)

	output = run("o", "e", "1", "", "", "", "Updated backup", "y", "q", "q")
	cfg = readConfig()
	profile, err = cfg.GetSection("notify:telegram-me")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"provider": "telegram",
		"token":    storedToken,
		"chat_id":  "-12345",
		"label":    "Updated backup",
	}, profile)
	assert.NotContains(t, output, storedToken)

	output = run("o", "t", "2s", "20s", "30s", "y", "q", "q")
	cfg = readConfig()
	timing, err := cfg.GetSection("notify:")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"interval": "2s", "request_timeout": "20s", "shutdown_timeout": "30s",
	}, timing)
	unchangedProfile, err := cfg.GetSection("notify:telegram-me")
	require.NoError(t, err)
	assert.Equal(t, profile, unchangedProfile)
	assert.Contains(t, output, "Notification timing saved.")

	output = run("o", "d", "1", "y", "q", "q")
	cfg = readConfig()
	assert.NotContains(t, cfg.GetSectionList(), "notify:telegram-me")
	assert.NotContains(t, output, storedToken)
	remainingTiming, err := cfg.GetSection("notify:")
	require.NoError(t, err)
	assert.Equal(t, timing, remainingTiming)
	assert.Contains(t, output, "No notification profiles configured.")
}
