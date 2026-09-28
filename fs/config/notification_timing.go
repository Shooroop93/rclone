package config

import (
	"fmt"
	"strings"
	"time"
)

// NotificationSettingsSection stores settings shared by all notification profiles.
const NotificationSettingsSection = NotificationSectionPrefix

// NotificationTiming controls progress updates and delivery deadlines for all profiles.
type NotificationTiming struct {
	// Interval is the time between progress snapshots.
	Interval time.Duration
	// RequestTimeout is the maximum duration of one notification request.
	RequestTimeout time.Duration
	// ShutdownTimeout is the total time allowed for final delivery to all profiles.
	ShutdownTimeout time.Duration
}

func defaultNotificationTiming() NotificationTiming {
	return NotificationTiming{
		Interval:        5 * time.Second,
		RequestTimeout:  10 * time.Second,
		ShutdownTimeout: 15 * time.Second,
	}
}

// ReadNotificationTiming reads shared timings from the config file, using defaults for missing values.
// It returns all defaults and an error if any configured duration is invalid or not positive.
func ReadNotificationTiming() (NotificationTiming, error) {
	timing := defaultNotificationTiming()
	for _, setting := range []struct {
		key   string
		value *time.Duration
	}{
		{"interval", &timing.Interval},
		{"request_timeout", &timing.RequestTimeout},
		{"shutdown_timeout", &timing.ShutdownTimeout},
	} {
		value, _ := FileGetValue(NotificationSettingsSection, setting.key)
		if value == "" {
			continue
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return defaultNotificationTiming(), fmt.Errorf("notification setting %s in [%s] must be a positive duration", setting.key, NotificationSettingsSection)
		}
		*setting.value = duration
	}
	return timing, nil
}

func readNotificationDuration(key, description string, previous time.Duration) time.Duration {
	fmt.Println(description)
	fmt.Printf("Enter a positive duration, for example 5s or 1m. Press Enter to keep %s.\n", previous)
	for {
		value := strings.TrimSpace(ReadLine(key + "> "))
		if value == "" {
			return previous
		}
		duration, err := time.ParseDuration(value)
		if err == nil && duration > 0 {
			return duration
		}
		fmt.Println("Enter a positive duration, for example 5s or 1m.")
	}
}

func editNotificationTiming() error {
	timing, err := ReadNotificationTiming()
	if err != nil {
		fmt.Printf("%v. Using default timings.\n", err)
	}
	fmt.Println("Notification timing applies to all profiles.")
	timing.Interval = readNotificationDuration("interval", "Time between progress snapshots.", timing.Interval)
	timing.RequestTimeout = readNotificationDuration("request_timeout", "Maximum duration of one notification request.", timing.RequestTimeout)
	timing.ShutdownTimeout = readNotificationDuration("shutdown_timeout", "Total time allowed for final delivery to all profiles.", timing.ShutdownTimeout)
	fmt.Printf("\nProgress interval: %s\nRequest timeout: %s\nShutdown timeout: %s\n", timing.Interval, timing.RequestTimeout, timing.ShutdownTimeout)
	fmt.Println("Save notification timing?")
	if !Confirm(true) {
		return nil
	}
	if err := saveNotificationProfile(NotificationSettingsSection, map[string]string{
		"interval": timing.Interval.String(), "request_timeout": timing.RequestTimeout.String(), "shutdown_timeout": timing.ShutdownTimeout.String(),
	}); err != nil {
		return err
	}
	fmt.Println("Notification timing saved.")
	return nil
}
