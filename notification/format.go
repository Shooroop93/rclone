package notification

import (
	"fmt"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
)

// FormatMessage returns a plain-text notification for the operation snapshot.
// It returns an error if state.Phase is not start, running, or end.
func FormatMessage(state State) (string, error) {
	var status string
	switch state.Phase {
	case "start":
		status = "🟡 rclone · %s — started"
	case "running":
		status = "🔄 rclone · %s — in progress"
	case "end":
		status = "✅ rclone · %s — completed successfully"
		if state.Error != nil {
			status = "❌ rclone · %s — failed"
		}
	default:
		return "", fmt.Errorf("unknown state: %s", state.Phase)
	}

	lines := []string{fmt.Sprintf(status, state.Operation)}
	if state.Source != "" || state.Destination != "" || state.Profile != "" {
		lines = append(lines, "")
	}
	if state.Source != "" {
		lines = append(lines, "Source: "+state.Source)
	}
	if state.Destination != "" {
		lines = append(lines, "Destination: "+state.Destination)
	}
	if state.Profile != "" {
		lines = append(lines, "Profile: "+state.Profile)
	}

	if state.Phase == "start" {
		lines = append(lines, "", "Started: "+formatTime(state.StartedAt))
		return strings.Join(lines, "\n"), nil
	}

	transferred := fs.SizeSuffix(state.Bytes).ByteUnit()
	transfers := fmt.Sprintf("%d", state.Transfers)
	if state.Phase == "running" {
		if state.TotalBytes > 0 {
			transferred += " / ≈" + fs.SizeSuffix(state.TotalBytes).ByteUnit()
		}
		if state.TotalTransfers > 0 {
			transfers += fmt.Sprintf(" / %d", state.TotalTransfers)
		}
	}
	lines = append(lines,
		"",
		"Transferred: "+transferred,
		"Transfers: "+transfers,
		fmt.Sprintf("Checked: %d", state.Checks),
	)
	if state.Deletes > 0 || state.DeletedDirs > 0 {
		lines = append(lines, fmt.Sprintf("Deleted: %d files, %d directories", state.Deletes, state.DeletedDirs))
	}

	if state.Phase == "running" {
		eta := "unknown"
		if state.ETAKnown && state.ETA >= 0 {
			eta = "≈" + formatDuration(state.ETA)
		}
		lines = append(lines,
			"Speed: "+fs.SizeSuffix(state.Speed).ByteRateUnit(),
			"ETA: "+eta,
			"",
			"Elapsed: "+formatDuration(state.Elapsed),
		)
	} else {
		averageSpeed := "unknown"
		if state.Elapsed > 0 {
			averageSpeed = fs.SizeSuffix(float64(state.Bytes) / state.Elapsed.Seconds()).ByteRateUnit()
		}
		maxSpeed := "unknown"
		if state.MaxSpeedKnown {
			maxSpeed = fs.SizeSuffix(state.MaxSpeed).ByteRateUnit()
		}
		lines = append(lines,
			"Average speed: "+averageSpeed,
			"Max speed (observed): "+maxSpeed,
		)
		if state.Error != nil {
			lines = append(lines, "", fmt.Sprintf("Error: %v", state.Error))
		}
		lines = append(lines,
			"",
			"Duration: "+formatDuration(state.Elapsed),
			"Finished: "+formatTime(state.FinishedAt),
		)
	}

	return strings.Join(lines, "\n"), nil
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.Format("2006-01-02 15:04:05 -07:00")
}

func formatDuration(value time.Duration) string {
	return fs.Duration(value.Truncate(time.Second)).ReadableString()
}
