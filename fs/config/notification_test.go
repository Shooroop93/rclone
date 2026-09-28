package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func notificationTestConfig(t *testing.T) {
	t.Helper()
	oldData, oldLoaded := data, dataLoaded
	data, dataLoaded = newDefaultStorage(), true
	t.Cleanup(func() { data, dataLoaded = oldData, oldLoaded })
}

func notificationTestOutput(t *testing.T, fn func()) string {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	require.NoError(t, err)
	oldStdout := os.Stdout
	os.Stdout = out
	defer func() {
		os.Stdout = oldStdout
		assert.NoError(t, out.Close())
	}()
	fn()
	contents, err := os.ReadFile(out.Name())
	require.NoError(t, err)
	return string(contents)
}

func TestNotificationConfigRemotes(t *testing.T) {
	notificationTestConfig(t)
	FileSetValue("remote", "type", "local")
	FileSetValue("notify.legacy", "type", "local")
	FileSetValue("notify:profile", "provider", "telegram")
	FileSetValue("notify:profile", "type", "local")
	t.Setenv(fs.ConfigToEnv("notify:environment", "type"), "local")
	t.Setenv(fs.ConfigToEnv("notification_environment", "type"), "local")

	names := GetRemoteNames()
	assert.Contains(t, names, "remote")
	assert.Contains(t, names, "notify.legacy")
	assert.Contains(t, names, "notification_environment")
	assert.NotContains(t, names, "notify:profile")
	assert.Contains(t, names, "notify:environment", "notification profiles must not change environment remote discovery")
	assert.Contains(t, FileSections(), "notify:profile")
	FileSetValue("incomplete", "description", "remote without a type")
	output := notificationTestOutput(t, ShowRemotes)
	assert.Contains(t, output, "incomplete")
	assert.NotContains(t, output, "notify:profile")
	assert.ElementsMatch(t, []string{"remote", "notify.legacy", "incomplete", "notify:profile"}, FileSections())
}

func TestNotificationConfigRemoteUI(t *testing.T) {
	notificationTestConfig(t)
	FileSetValue("remote", "type", "local")
	FileSetValue("notify:profile", "provider", "telegram")
	oldReadLine := ReadLine
	t.Cleanup(func() { ReadLine = oldReadLine })
	ReadLine = func(string) string { return "1" }

	output := notificationTestOutput(t, func() {
		ShowRemotes()
		assert.Equal(t, "remote", ChooseRemote())
	})
	assert.Contains(t, output, "remote")
	assert.NotContains(t, output, "notify:profile")

	LoadedData().DeleteSection("remote")
	ReadLine = func(string) string { return "q" }
	output = notificationTestOutput(t, func() {
		require.NoError(t, EditConfig(context.Background()))
	})
	assert.Contains(t, output, "No remotes found")
	assert.NotContains(t, output, "notify:profile")
}

func TestNotificationConfigPreservesRemoteBehavior(t *testing.T) {
	notificationTestConfig(t)
	registry := fs.Registry
	t.Cleanup(func() { fs.Registry = registry })
	fs.Registry = append(append([]*fs.RegInfo(nil), registry...), &fs.RegInfo{
		Name: "notification_compatibility",
		Options: fs.Options{
			{Name: "password", IsPassword: true},
			{Name: "secret", Sensitive: true},
		},
	})
	names := []string{"remote", "notify", "notify.legacy", "notify-team"}
	for _, name := range names {
		FileSetValue(name, "type", "notification_compatibility")
		FileSetValue(name, "description", "Existing remote")
		FileSetValue(name, "password", "existing-password")
		FileSetValue(name, "secret", "existing-secret")
	}
	t.Setenv(fs.ConfigToEnv("notify", "type"), "notification_compatibility")
	remotes := GetRemotes()
	assert.Contains(t, remotes, Remote{Name: "notify", Type: "notification_compatibility", Source: "environment"})
	show := func() {
		ShowRemotes()
		for _, name := range names {
			ShowRemote(name)
			ShowRedactedRemote(name)
		}
	}
	before := notificationTestOutput(t, show)
	assert.Contains(t, before, "password = *** ENCRYPTED ***")
	assert.Contains(t, before, "password = XXX")
	assert.Contains(t, before, "secret = XXX")
	assert.NotContains(t, before, "existing-password")
	FileSetValue("notify:profile", "provider", "telegram")
	FileSetValue("notify:profile", "token", "123:synthetic_token")
	FileSetValue(NotificationSettingsSection, "interval", "5s")
	assert.ElementsMatch(t, remotes, GetRemotes())
	assert.ElementsMatch(t, strings.Split(before, "\n"), strings.Split(notificationTestOutput(t, show), "\n"))
	assert.Contains(t, FileSections(), "notify:profile")
	assert.Contains(t, FileSections(), NotificationSettingsSection)
}

func TestNotificationConfigRedacted(t *testing.T) {
	notificationTestConfig(t)
	FileSetValue("notify:profile", "provider", "telegram")
	FileSetValue("notify:profile", "token", "secret-token")
	FileSetValue("notify:profile", "chat_id", "secret-chat")
	FileSetValue("notify:profile", "future_secret", "future-value")
	FileSetValue("notify:profile", "empty", "")

	output := notificationTestOutput(t, ShowRedactedConfig)
	assert.Contains(t, output, "[notify:profile]\n")
	assert.Contains(t, output, "provider = telegram\n")
	assert.Contains(t, output, "token = XXX\n")
	assert.Contains(t, output, "chat_id = XXX\n")
	assert.Contains(t, output, "future_secret = XXX\n")
	assert.Contains(t, output, "empty = \n")
	assert.NotContains(t, output, "secret-token")
	assert.NotContains(t, output, "secret-chat")
	assert.NotContains(t, output, "future-value")
	assert.NotContains(t, output, "couldn't find type")

	output = notificationTestOutput(t, func() { ShowRemote("notify:profile") })
	assert.Contains(t, output, "token = secret-token\n")
	assert.Contains(t, output, "chat_id = secret-chat\n")
	assert.Equal(t, "secret-token", DumpRcRemote("notify:profile")["token"])
	assert.Contains(t, DumpRcBlob(), "notify:profile")
}
