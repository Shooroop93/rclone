package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/notification"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotificationTimingFlagsRemoved(t *testing.T) {
	for _, name := range []string{"notify-interval", "notify-request-timeout", "notify-shutdown-timeout"} {
		assert.Nil(t, pflag.Lookup(name), "notification timing must be configured through the config menu")
	}
	assert.NotNil(t, pflag.Lookup("notify"), "profile selection must remain available")
}

func TestNotificationRemoteCompletion(t *testing.T) {
	notificationTestConfig(t, "[notify]\ntype=local\n\n[notify.legacy]\ntype=local\n\n[ordinary]\ntype=local\n")
	before := addRemotes("", []string{"existing-completion"})
	assert.Equal(t, []string{"existing-completion", "notify:", "notify.legacy:", "ordinary:"}, before)
	config.FileSetValue("notify:profile", "provider", "telegram")
	config.FileSetValue(config.NotificationSettingsSection, "interval", "5s")
	assert.Equal(t, before, addRemotes("", []string{"existing-completion"}))
	assert.Equal(t, []string{"notify:", "notify.legacy:"}, addRemotes("notify", nil))
}

func notificationTestConfig(t *testing.T, contents string) {
	t.Helper()
	oldPath, oldData := config.GetConfigPath(), config.Data()
	filename := filepath.Join(t.TempDir(), "rclone.conf")
	require.NoError(t, os.WriteFile(filename, []byte(contents), 0600))
	require.NoError(t, config.SetConfigPath(filename))
	configfile.Install()
	t.Cleanup(func() {
		config.SetData(oldData)
		require.NoError(t, config.SetConfigPath(oldPath))
	})
}

func TestReadNotificationProfile(t *testing.T) {
	secret, err := obscure.Obscure("123:secret")
	require.NoError(t, err)
	notificationTestConfig(t, `[notify:personal]
provider = telegram
token = 123:secret
chat_id = -12345
label = Nightly backup

[notify:obscured]
provider = telegram
token = obscured:`+secret+`
chat_id = @channel

[notify:missing-token]
provider = telegram
chat_id = -12345

[notify:missing-chat]
provider = telegram
token = 123:secret

[notify:unknown]
provider = unknown
token = 123:secret

[notify:broken-secret]
provider = telegram
token = obscured:invalid!
chat_id = -12345

[notify:unsafe-token]
provider = telegram
token = 123:secret/path
chat_id = -12345
`)

	profile, err := readNotificationProfile("personal")
	require.NoError(t, err)
	assert.Equal(t, "123:secret", profile.token)
	assert.Equal(t, "-12345", profile.chatID)
	assert.Equal(t, "Nightly backup", profile.label)
	profile, err = readNotificationProfile("obscured")
	require.NoError(t, err)
	assert.Equal(t, "123:secret", profile.token)
	assert.Equal(t, "@channel", profile.chatID)

	for _, name := range []string{"missing", "missing-token", "missing-chat", "unknown", "broken-secret", "unsafe-token", "", "notify:personal"} {
		t.Run(name, func(t *testing.T) {
			_, err := readNotificationProfile(name)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "123:secret")
			assert.NotContains(t, err.Error(), "invalid!")
		})
	}
}

func TestNotificationProfileSelection(t *testing.T) {
	notificationTestConfig(t, `[notify:personal]
provider = telegram
token = 123:secret
chat_id = -12345

[notify:team]
provider = telegram
token = 456:secret
chat_id = -67890
`)
	oldProfiles := *notifyProfiles
	t.Cleanup(func() { *notifyProfiles = oldProfiles })
	command := &cobra.Command{Use: "copy", Annotations: map[string]string{"notification": "true"}}
	*notifyProfiles = []string{"personal", "missing", "personal", "team"}
	notifiers, options := notificationNotifiers(context.Background(), command)
	require.Len(t, notifiers, 2, "a bad profile must not disable other profiles, and duplicates must be removed")
	assert.Equal(t, notification.Options{Interval: 5 * time.Second, RequestTimeout: 10 * time.Second, ShutdownTimeout: 15 * time.Second}, options)
	assert.NotSame(t, notifiers[0], notifiers[1])
	nextNotifiers, _ := notificationNotifiers(context.Background(), command)
	assert.NotSame(t, notifiers[0], nextNotifiers[0], "each run needs a fresh session")
	command.Annotations = nil
	notifiers, options = notificationNotifiers(context.Background(), command)
	assert.Empty(t, notifiers)
	assert.Equal(t, notification.Options{}, options)
	*notifyProfiles = nil
	command.Annotations = map[string]string{"notification": "true"}
	notifiers, options = notificationNotifiers(context.Background(), command)
	assert.Empty(t, notifiers)
	assert.Equal(t, notification.Options{}, options)
}

func TestNotificationConfiguredTiming(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings string
		want     notification.Options
	}{
		{
			name:     "configured",
			settings: "interval = 30s\nrequest_timeout = 7s\nshutdown_timeout = 22s\n",
			want:     notification.Options{Interval: 30 * time.Second, RequestTimeout: 7 * time.Second, ShutdownTimeout: 22 * time.Second},
		},
		{
			name:     "invalid",
			settings: "interval = invalid\nrequest_timeout = 7s\nshutdown_timeout = 22s\n",
			want:     notification.Options{Interval: 5 * time.Second, RequestTimeout: 10 * time.Second, ShutdownTimeout: 15 * time.Second},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			notificationTestConfig(t, "[notify:]\n"+test.settings+`
[notify:personal]
provider = telegram
token = 123:secret
chat_id = -12345
`)
			oldProfiles := *notifyProfiles
			t.Cleanup(func() { *notifyProfiles = oldProfiles })
			*notifyProfiles = []string{"personal"}
			command := &cobra.Command{Use: "copy", Annotations: map[string]string{"notification": "true"}}
			notifiers, options := notificationNotifiers(context.Background(), command)
			require.Len(t, notifiers, 1, "invalid timing must not disable valid profiles")
			assert.Equal(t, test.want, options)
		})
	}
}

type profileTestNotifier func(context.Context, notification.State) error

func (f profileTestNotifier) Notify(ctx context.Context, state notification.State) error {
	return f(ctx, state)
}

func TestProfileNotifier(t *testing.T) {
	var got notification.State
	deliveryErr := errors.New("delivery failed")
	p := &profileNotifier{
		name: "personal",
		notifier: profileTestNotifier(func(_ context.Context, state notification.State) error {
			got = state
			return deliveryErr
		}),
	}
	state := notification.State{Phase: "start", Profile: "caller label"}
	err := p.Notify(context.Background(), state)
	assert.ErrorIs(t, err, deliveryErr)
	assert.ErrorContains(t, err, "personal")
	assert.Equal(t, state, got, "an absent profile label must preserve caller metadata")
	p.label = "Nightly backup"
	assert.ErrorIs(t, p.Notify(context.Background(), state), deliveryErr)
	assert.Equal(t, "Nightly backup", got.Profile)
	assert.Equal(t, "caller label", state.Profile)
	p.notifier = profileTestNotifier(func(context.Context, notification.State) error { return nil })
	assert.NoError(t, p.Notify(context.Background(), state))
}

func TestNotificationPath(t *testing.T) {
	for _, test := range []struct{ path, want string }{
		{`C:\source folder`, `C:\source folder`},
		{"remote:path/to/file", "remote:path/to/file"},
		{":sftp,host=example.com,pass=secret:/backup", ":sftp:/backup"},
		{"remote,token='secret:value':folder", "remote:folder"},
		{":local:/backup", ":local:/backup"},
	} {
		assert.Equal(t, test.want, notificationPath(test.path))
	}
}
