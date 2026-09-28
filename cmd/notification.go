package cmd

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/flags"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/fspath"
	"github.com/rclone/rclone/notification"
	"github.com/rclone/rclone/notification/telegram"
	"github.com/spf13/cobra"
)

var (
	notifyProfiles  = flags.StringArrayP("notify", "", nil, "Notification profile from rclone.conf for copy (repeat for multiple profiles)", "Copy,Logging")
	telegramTokenRE = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)
)

type notificationProfile struct {
	token  string
	chatID string
	label  string
}

func readNotificationProfile(name string) (notificationProfile, error) {
	var profile notificationProfile
	if err := fspath.CheckConfigName(name); err != nil {
		return profile, fmt.Errorf("invalid profile name: %w", err)
	}
	section := config.NotificationSectionPrefix + name
	provider, _ := config.FileGetValue(section, "provider")
	if provider == "" {
		return profile, fmt.Errorf("section [%s] is missing or has no provider", section)
	}
	if provider != "telegram" {
		return profile, fmt.Errorf("unsupported notification provider %q", provider)
	}
	profile.token, _ = config.FileGetValue(section, "token")
	profile.chatID, _ = config.FileGetValue(section, "chat_id")
	profile.label, _ = config.FileGetValue(section, "label")
	if obscured, ok := strings.CutPrefix(profile.token, "obscured:"); ok {
		var err error
		profile.token, err = obscure.Reveal(obscured)
		if err != nil {
			return notificationProfile{}, fmt.Errorf("invalid obscured token: %w", err)
		}
	}
	if !telegramTokenRE.MatchString(profile.token) {
		return notificationProfile{}, fmt.Errorf("token is missing or invalid")
	}
	if strings.TrimSpace(profile.chatID) == "" {
		return notificationProfile{}, fmt.Errorf("chat_id is missing")
	}
	return profile, nil
}

type profileNotifier struct {
	name     string
	label    string
	notifier notification.Notifier
}

func (p *profileNotifier) Notify(ctx context.Context, state notification.State) error {
	if p.label != "" {
		state.Profile = p.label
	}
	if err := p.notifier.Notify(ctx, state); err != nil {
		return fmt.Errorf("notification profile %q: %w", p.name, err)
	}
	return nil
}

func notificationNotifiers(ctx context.Context, command *cobra.Command) ([]notification.Notifier, notification.Options) {
	if len(*notifyProfiles) == 0 {
		return nil, notification.Options{}
	}
	if command.Annotations["notification"] != "true" {
		fs.Logf(nil, "notification: --notify is not supported by %s", command.Name())
		return nil, notification.Options{}
	}
	timing, err := config.ReadNotificationTiming()
	if err != nil {
		fs.Logf(nil, "notification: using default timing: %v", err)
	}
	options := notification.Options{
		Interval: timing.Interval, RequestTimeout: timing.RequestTimeout, ShutdownTimeout: timing.ShutdownTimeout,
	}
	var notifiers []notification.Notifier
	seen := make(map[string]bool)
	for _, name := range *notifyProfiles {
		if seen[name] {
			continue
		}
		seen[name] = true
		profile, err := readNotificationProfile(name)
		if err != nil {
			fs.Logf(nil, "notification: skipping profile %q: %v", name, err)
			continue
		}
		notifiers = append(notifiers, &profileNotifier{
			name: name, label: profile.label,
			notifier: telegram.NewSessionWithTimeout(ctx, profile.token, profile.chatID, options.RequestTimeout),
		})
	}
	return notifiers, options
}

func notificationPath(remote string) string {
	parsed, err := fspath.Parse(remote)
	if err != nil {
		return "[unavailable]"
	}
	// Connection-string parameters can contain backend credentials.
	if len(parsed.Config) != 0 {
		return parsed.Name + ":" + parsed.Path
	}
	return remote
}

func finishNotifications(manager *notification.Manager, result error) {
	if err := manager.Finish(context.Background(), result); err != nil {
		fs.Logf(nil, "notification: final delivery failed: %v", err)
	}
}
