package config

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadNotificationTiming(t *testing.T) {
	defaults := NotificationTiming{Interval: 5 * time.Second, RequestTimeout: 10 * time.Second, ShutdownTimeout: 15 * time.Second}
	for _, test := range []struct {
		name   string
		values map[string]string
		want   NotificationTiming
	}{
		{"absent", nil, defaults},
		{"empty", map[string]string{"interval": "", "request_timeout": "", "shutdown_timeout": ""}, defaults},
		{"custom", map[string]string{"interval": "250ms", "request_timeout": "2m", "shutdown_timeout": "1m30s"}, NotificationTiming{250 * time.Millisecond, 2 * time.Minute, 90 * time.Second}},
		{"partial", map[string]string{"interval": "30s"}, NotificationTiming{30 * time.Second, 10 * time.Second, 15 * time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			notificationTestConfig(t)
			for key, value := range test.values {
				FileSetValue(NotificationSettingsSection, key, value)
			}
			got, err := ReadNotificationTiming()
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
	for _, key := range []string{"interval", "request_timeout", "shutdown_timeout"} {
		for _, value := range []string{"invalid-duration", "0s", "-1s", "999999999999999999999h"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				notificationTestConfig(t)
				for _, validKey := range []string{"interval", "request_timeout", "shutdown_timeout"} {
					FileSetValue(NotificationSettingsSection, validKey, "30s")
				}
				FileSetValue(NotificationSettingsSection, key, value)
				got, err := ReadNotificationTiming()
				require.ErrorContains(t, err, key)
				assert.NotContains(t, err.Error(), value)
				assert.Equal(t, defaults, got)
			})
		}
	}
}

func TestNotificationUITiming(t *testing.T) {
	for _, test := range []struct {
		name    string
		stored  map[string]string
		answers []string
		want    map[string]string
		saves   int
	}{
		{"defaults", nil, []string{"", "", "", ""}, map[string]string{"interval": "5s", "request_timeout": "10s", "shutdown_timeout": "15s"}, 1},
		{"custom", nil, []string{"2s", "4s", "6s", "y"}, map[string]string{"interval": "2s", "request_timeout": "4s", "shutdown_timeout": "6s"}, 1},
		{"keep existing", map[string]string{"interval": "7s", "request_timeout": "8s", "shutdown_timeout": "9s"}, []string{"", "", "", "y"}, map[string]string{"interval": "7s", "request_timeout": "8s", "shutdown_timeout": "9s"}, 1},
		{"cancel new", nil, []string{"2s", "4s", "6s", "n"}, nil, 0},
		{"cancel existing", map[string]string{"interval": "7s"}, []string{"2s", "4s", "6s", "n"}, map[string]string{"interval": "7s"}, 0},
		{"replace invalid", map[string]string{"interval": "invalid"}, []string{"", "", "", "y"}, map[string]string{"interval": "5s", "request_timeout": "10s", "shutdown_timeout": "15s"}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := notificationUIConfig(t)
			for key, value := range test.stored {
				FileSetValue(NotificationSettingsSection, key, value)
			}
			answers := append([]string{"o", "t"}, test.answers...)
			answers = append(answers, "q", "q")
			output, err := runNotificationUI(t, answers...)
			require.NoError(t, err)
			assert.Contains(t, output, "t) Notification timing")
			assert.Equal(t, test.saves, storage.saves)
			assert.Equal(t, len(test.want), len(storage.GetKeyList(NotificationSettingsSection)))
			for key, value := range test.want {
				assert.Equal(t, value, GetValue(NotificationSettingsSection, key))
			}
			assert.Empty(t, notificationProfileNames())
			assert.Empty(t, notificationTestOutput(t, ShowRemotes))
		})
	}
}

func TestNotificationUITimingValidation(t *testing.T) {
	storage := notificationUIConfig(t)
	output, err := runNotificationUI(t,
		"o", "t", "invalid", "0s", "-1s", "1s", "0", "2s", "-2s", "3s", "y", "q", "q",
	)
	require.NoError(t, err)
	assert.Contains(t, output, "positive duration")
	assert.Equal(t, "1s", GetValue(NotificationSettingsSection, "interval"))
	assert.Equal(t, "2s", GetValue(NotificationSettingsSection, "request_timeout"))
	assert.Equal(t, "3s", GetValue(NotificationSettingsSection, "shutdown_timeout"))
	assert.Equal(t, 1, storage.saves)
}

func TestNotificationUITimingSaveFailure(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			storage := notificationUIConfig(t)
			storage.saveErr = errors.New("config save failed")
			if existing {
				FileSetValue(NotificationSettingsSection, "interval", "7s")
				FileSetValue(NotificationSettingsSection, "other_key", "retained")
			}
			before, err := storage.Serialize()
			require.NoError(t, err)
			_, err = runNotificationUI(t, "o", "t", "1s", "2s", "3s", "y")
			require.ErrorIs(t, err, storage.saveErr)
			after, err := storage.Serialize()
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Equal(t, 1, storage.saves)
		})
	}
}

func TestNotificationUITimingWithProfiles(t *testing.T) {
	storage := notificationUIConfig(t)
	FileSetValue(NotificationSettingsSection, "interval", "7s")
	FileSetValue("notify:profile", "provider", "telegram")
	assert.Equal(t, []string{"profile"}, notificationProfileNames())
	output, err := runNotificationUI(t,
		"o", "t", "", "", "", "y", "d", "1", "y", "q", "q",
	)
	require.NoError(t, err)
	assert.Contains(t, output, "t) Notification timing")
	assert.False(t, storage.HasSection("notify:profile"))
	assert.True(t, storage.HasSection(NotificationSettingsSection))
	assert.Equal(t, "7s", GetValue(NotificationSettingsSection, "interval"))
	assert.Equal(t, 2, storage.saves)
	assert.Empty(t, notificationProfileNames())
}
