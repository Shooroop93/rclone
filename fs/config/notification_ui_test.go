package config

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type notificationUIStorage struct {
	*defaultStorage
	saves   int
	saveErr error
}

func (s *notificationUIStorage) Save() error {
	s.saves++
	return s.saveErr
}

func notificationUIConfig(t *testing.T) *notificationUIStorage {
	t.Helper()
	notificationTestConfig(t)
	storage := &notificationUIStorage{defaultStorage: newDefaultStorage()}
	data = storage
	return storage
}

func runNotificationUI(t *testing.T, answers ...string) (string, error) {
	t.Helper()
	stdin, err := os.Open(os.DevNull)
	require.NoError(t, err)
	oldStdin, oldReadLine := os.Stdin, ReadLine
	os.Stdin = stdin
	defer func() {
		os.Stdin, ReadLine = oldStdin, oldReadLine
		assert.NoError(t, stdin.Close())
	}()
	index := 0
	ReadLine = func(prompt string) string {
		require.Less(t, index, len(answers), "unexpected prompt %q", prompt)
		answer := answers[index]
		index++
		return answer
	}
	var editErr error
	output := notificationTestOutput(t, func() {
		editErr = EditConfig(context.Background())
	})
	assert.Equal(t, len(answers), index, "unconsumed answers")
	return output, editErr
}

func notificationUIToken(t *testing.T, section, expected string) {
	t.Helper()
	stored, found := FileGetValue(section, "token")
	require.True(t, found)
	value, prefixed := strings.CutPrefix(stored, "obscured:")
	require.True(t, prefixed, "token must be stored obscured")
	revealed, err := obscure.Reveal(value)
	require.NoError(t, err)
	assert.Equal(t, expected, revealed)
}

func TestNotificationUICreate(t *testing.T) {
	storage := notificationUIConfig(t)
	const token = "123456:synthetic_token"
	output, err := runNotificationUI(t,
		"o", "n", "telegram-me", "", token, "123456789", "My notifications", "y", "q", "q",
	)
	require.NoError(t, err)
	assert.Equal(t, "telegram", GetValue("notify:telegram-me", "provider"))
	assert.Equal(t, "123456789", GetValue("notify:telegram-me", "chat_id"))
	assert.Equal(t, "My notifications", GetValue("notify:telegram-me", "label"))
	notificationUIToken(t, "notify:telegram-me", token)
	assert.Equal(t, 1, storage.saves)
	assert.Empty(t, notificationTestOutput(t, ShowRemotes))
	assert.NotContains(t, output, token)
}

func TestNotificationUICreateCanceled(t *testing.T) {
	storage := notificationUIConfig(t)
	output, err := runNotificationUI(t,
		"o", "n", "canceled", "1", "123456:canceled_token", "123456789", "", "n", "q", "q",
	)
	require.NoError(t, err)
	assert.False(t, storage.HasSection("notify:canceled"))
	assert.Zero(t, storage.saves)
	assert.NotContains(t, output, "123456:canceled_token")
}

func TestNotificationUIValidation(t *testing.T) {
	storage := notificationUIConfig(t)
	FileSetValue("notify:existing", "provider", "telegram")
	const token = "123456:valid_token"
	output, err := runNotificationUI(t,
		"o", "n", "", "bad:name", "existing", "new-profile", "",
		"", "invalid-token", token, "", "123456789", "", "y", "q", "q",
	)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"notify:existing", "notify:new-profile"}, FileSections())
	notificationUIToken(t, "notify:new-profile", token)
	assert.Equal(t, "123456789", GetValue("notify:new-profile", "chat_id"))
	assert.Equal(t, 1, storage.saves)
	assert.NotContains(t, output, token)
	assert.NotContains(t, output, "invalid-token")
}

func TestNotificationUIEdit(t *testing.T) {
	const oldToken = "123456:old_token"
	const newToken = "654321:new_token"
	for _, test := range []struct {
		name       string
		stored     string
		token      string
		chatID     string
		label      string
		confirm    string
		wantToken  string
		wantChatID string
		wantLabel  string
		wantSaves  int
	}{
		{"keep obscured", "obscured:" + obscure.MustObscure(oldToken), "", "", "", "y", oldToken, "123456789", "Original label", 1},
		{"keep plaintext", oldToken, "", "", "", "y", oldToken, "123456789", "Original label", 1},
		{"replace", "obscured:" + obscure.MustObscure(oldToken), newToken, "987654321", "New label", "y", newToken, "987654321", "New label", 1},
		{"clear label", "obscured:" + obscure.MustObscure(oldToken), "", "", "-", "y", oldToken, "123456789", "", 1},
		{"cancel", "obscured:" + obscure.MustObscure(oldToken), newToken, "987654321", "New label", "n", oldToken, "123456789", "Original label", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := notificationUIConfig(t)
			FileSetValue("notify:profile", "provider", "telegram")
			FileSetValue("notify:profile", "token", test.stored)
			FileSetValue("notify:profile", "chat_id", "123456789")
			FileSetValue("notify:profile", "label", "Original label")
			FileSetValue("remote", "type", "local")
			output, err := runNotificationUI(t,
				"o", "e", "1", "", test.token, test.chatID, test.label, test.confirm, "q", "q",
			)
			require.NoError(t, err)
			assert.Equal(t, "telegram", GetValue("notify:profile", "provider"))
			assert.Equal(t, test.wantChatID, GetValue("notify:profile", "chat_id"))
			assert.Equal(t, test.wantLabel, GetValue("notify:profile", "label"))
			if test.token == "" || test.confirm == "n" {
				assert.Equal(t, test.stored, GetValue("notify:profile", "token"))
			} else {
				notificationUIToken(t, "notify:profile", test.wantToken)
			}
			assert.Equal(t, test.wantSaves, storage.saves)
			assert.Equal(t, "local", GetValue("remote", "type"))
			assert.NotContains(t, output, oldToken)
			assert.NotContains(t, output, newToken)
			assert.NotContains(t, output, test.stored)
		})
	}
}

func TestNotificationUIDelete(t *testing.T) {
	for _, test := range []struct {
		name    string
		confirm string
		deleted bool
		saves   int
	}{
		{"cancel", "n", false, 0},
		{"default cancels", "", false, 0},
		{"confirm", "y", true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := notificationUIConfig(t)
			FileSetValue("notify:profile", "provider", "telegram")
			FileSetValue("notify:profile", "token", "123456:delete_token")
			FileSetValue("remote", "type", "local")
			output, err := runNotificationUI(t, "o", "d", "profile", test.confirm, "q", "q")
			require.NoError(t, err)
			assert.Equal(t, !test.deleted, storage.HasSection("notify:profile"))
			assert.Equal(t, test.saves, storage.saves)
			assert.Equal(t, "local", GetValue("remote", "type"))
			assert.NotContains(t, output, "123456:delete_token")
		})
	}
}

func TestNotificationUISaveFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		answers []string
	}{
		{"create", []string{"o", "n", "new-profile", "", "123456:new_token", "123456789", "New label", "y"}},
		{"edit", []string{"o", "e", "1", "", "123456:new_token", "123456789", "New label", "y"}},
		{"delete", []string{"o", "d", "1", "y"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := notificationUIConfig(t)
			storage.saveErr = errors.New("config save failed")
			FileSetValue("notify:profile", "provider", "telegram")
			FileSetValue("notify:profile", "token", "123456:old_token")
			FileSetValue("notify:profile", "chat_id", "987654321")
			FileSetValue("notify:profile", "label", "Original label")
			FileSetValue("notify:profile", "other_key", "retained value")
			FileSetValue("remote", "type", "local")
			before, err := storage.Serialize()
			require.NoError(t, err)
			_, err = runNotificationUI(t, test.answers...)
			require.ErrorIs(t, err, storage.saveErr)
			after, err := storage.Serialize()
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Equal(t, 1, storage.saves)
		})
	}
}
