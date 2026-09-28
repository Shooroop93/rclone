package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/fspath"
)

var notificationTokenRE = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

func notificationProfileNames() []string {
	var names []string
	for _, section := range FileSections() {
		if name, ok := strings.CutPrefix(section, NotificationSectionPrefix); ok && name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func newNotificationName() string {
	for {
		fmt.Println("Enter a name for the notification profile, for example telegram-me.")
		name := ReadLine("name> ")
		if err := fspath.CheckConfigName(name); err != nil {
			fmt.Printf("Invalid profile name: %v.\n", err)
			continue
		}
		if LoadedData().HasSection(NotificationSectionPrefix + name) {
			fmt.Printf("Notification profile %q already exists.\n", name)
			continue
		}
		return name
	}
}

func readNotificationToken(previous string) (string, error) {
	fmt.Println("Enter the Telegram bot token. Input is hidden.")
	if previous != "" {
		fmt.Println("Press Enter to keep the existing token.")
	}
	for {
		_, _ = fmt.Fprint(PasswordPromptOutput, "token> ")
		token := strings.TrimSpace(ReadPassword())
		if token == "" && previous != "" {
			return previous, nil
		}
		if !notificationTokenRE.MatchString(token) {
			fmt.Println("Invalid bot token. Enter the token supplied for your Telegram bot.")
			continue
		}
		encoded, err := obscure.Obscure(token)
		if err != nil {
			return "", fmt.Errorf("obscure notification token: %w", err)
		}
		return "obscured:" + encoded, nil
	}
}

func saveNotificationProfile(section string, values map[string]string) error {
	storage := LoadedData()
	previous := make(map[string]string)
	for _, key := range storage.GetKeyList(section) {
		previous[key], _ = storage.GetValue(section, key)
	}
	if values == nil {
		storage.DeleteSection(section)
	} else {
		for key, value := range values {
			storage.SetValue(section, key, value)
		}
	}
	if err := storage.Save(); err != nil {
		// Keep the editor's state consistent if persistence fails.
		storage.DeleteSection(section)
		for key, value := range previous {
			storage.SetValue(section, key, value)
		}
		return fmt.Errorf("save notification configuration: %w", err)
	}
	return nil
}

func editNotificationProfile(name string) error {
	section := NotificationSectionPrefix + name
	previousToken, _ := FileGetValue(section, "token")
	previousChat, _ := FileGetValue(section, "chat_id")
	previousLabel, _ := FileGetValue(section, "label")
	fmt.Printf("Notification profile: %s\n", name)
	provider := Choose("provider", "provider", []string{"telegram"}, []string{"Telegram"}, "telegram", true, false)
	token, err := readNotificationToken(previousToken)
	if err != nil {
		return err
	}
	fmt.Println("Enter the destination chat ID or channel username (for example @mychannel).")
	chatID := Enter("chat_id", "string", previousChat, true)
	fmt.Println("Enter an optional label to display in notifications.")
	if previousLabel != "" {
		fmt.Printf("Press Enter to keep %q, or enter - to clear it.\n", previousLabel)
	}
	label := ReadLine("label> ")
	if label == "" {
		label = previousLabel
	} else if label == "-" {
		label = ""
	}
	fmt.Printf("\nProfile: %s\nProvider: %s\nToken: ***\nChat ID: %s\nLabel: %s\n", name, provider, chatID, label)
	fmt.Println("Save this notification profile?")
	if !Confirm(true) {
		return nil
	}
	if err := saveNotificationProfile(section, map[string]string{
		"provider": provider, "token": token, "chat_id": chatID, "label": label,
	}); err != nil {
		return err
	}
	fmt.Printf("Notification profile %q saved. Use --notify %q with copy.\n", name, name)
	return nil
}

func editNotificationConfig() error {
	for {
		names := notificationProfileNames()
		what := []string{"nNew notification profile", "tNotification timing", "qBack to config"}
		if len(names) == 0 {
			fmt.Println("No notification profiles configured.")
		} else {
			fmt.Printf("\n%-20s %s\n", "Name", "Provider")
			for _, name := range names {
				provider, _ := FileGetValue(NotificationSectionPrefix+name, "provider")
				fmt.Printf("%-20s %s\n", name, provider)
			}
			what = []string{"eEdit notification profile", "nNew notification profile", "dDelete notification profile", "tNotification timing", "qBack to config"}
		}
		switch Command(what) {
		case 't':
			if err := editNotificationTiming(); err != nil {
				return err
			}
		case 'n':
			if err := editNotificationProfile(newNotificationName()); err != nil {
				return err
			}
		case 'e':
			name := Choose("profile", "profile", names, nil, "", true, false)
			if err := editNotificationProfile(name); err != nil {
				return err
			}
		case 'd':
			name := Choose("profile", "profile", names, nil, "", true, false)
			fmt.Printf("Delete notification profile %q?\n", name)
			if Confirm(false) {
				if err := saveNotificationProfile(NotificationSectionPrefix+name, nil); err != nil {
					return err
				}
				fmt.Printf("Notification profile %q deleted.\n", name)
			}
		case 'q':
			return nil
		}
	}
}
