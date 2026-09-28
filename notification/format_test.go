package notification

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatMessage(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 9, 28, 12, 34, 56, 0, time.FixedZone("MSK", 3*60*60))
	state := State{
		Operation:      "sync",
		Source:         "local/фото",
		Destination:    "remote:backup",
		Profile:        "Daily backup",
		Bytes:          120 * 1024,
		TotalBytes:     240 * 1024,
		Transfers:      3,
		TotalTransfers: 6,
		Checks:         12,
		Deletes:        2,
		DeletedDirs:    1,
		Speed:          3 * 1024,
		MaxSpeed:       4 * 1024,
		MaxSpeedKnown:  true,
		ETA:            65*time.Second + 900*time.Millisecond,
		ETAKnown:       true,
		StartedAt:      startedAt,
		FinishedAt:     startedAt.Add(2 * time.Minute),
		Elapsed:        2 * time.Minute,
	}
	for _, test := range []struct {
		name  string
		phase string
		err   error
		want  string
	}{
		{
			name:  "start",
			phase: "start",
			want: `🟡 rclone · sync — started

Source: local/фото
Destination: remote:backup
Profile: Daily backup

Started: 2026-09-28 12:34:56 +03:00`,
		},
		{
			name:  "running",
			phase: "running",
			want: `🔄 rclone · sync — in progress

Source: local/фото
Destination: remote:backup
Profile: Daily backup

Transferred: 120 KiB / ≈240 KiB
Transfers: 3 / 6
Checked: 12
Deleted: 2 files, 1 directories
Speed: 3 KiB/s
ETA: ≈1m5s

Elapsed: 2m`,
		},
		{
			name:  "success",
			phase: "end",
			want: `✅ rclone · sync — completed successfully

Source: local/фото
Destination: remote:backup
Profile: Daily backup

Transferred: 120 KiB
Transfers: 3
Checked: 12
Deleted: 2 files, 1 directories
Average speed: 1 KiB/s
Max speed (observed): 4 KiB/s

Duration: 2m
Finished: 2026-09-28 12:36:56 +03:00`,
		},
		{
			name:  "failure",
			phase: "end",
			err:   errors.New("permission denied"),
			want: `❌ rclone · sync — failed

Source: local/фото
Destination: remote:backup
Profile: Daily backup

Transferred: 120 KiB
Transfers: 3
Checked: 12
Deleted: 2 files, 1 directories
Average speed: 1 KiB/s
Max speed (observed): 4 KiB/s

Error: permission denied

Duration: 2m
Finished: 2026-09-28 12:36:56 +03:00`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := state
			snapshot.Phase = test.phase
			snapshot.Error = test.err
			message, err := FormatMessage(snapshot)
			require.NoError(t, err)
			assert.Equal(t, test.want, message)
		})
	}
}

func TestFormatMessageMetadata(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		source      string
		destination string
		profile     string
		want        string
	}{
		{
			name: "none",
			want: "🟡 rclone · copy — started\n\nStarted: unknown",
		},
		{
			name:   "source",
			source: "/source",
			want:   "🟡 rclone · copy — started\n\nSource: /source\n\nStarted: unknown",
		},
		{
			name:        "destination",
			destination: "remote:destination",
			want:        "🟡 rclone · copy — started\n\nDestination: remote:destination\n\nStarted: unknown",
		},
		{
			name:    "profile",
			profile: "backup",
			want:    "🟡 rclone · copy — started\n\nProfile: backup\n\nStarted: unknown",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, err := FormatMessage(State{
				Operation:   "copy",
				Phase:       "start",
				Source:      test.source,
				Destination: test.destination,
				Profile:     test.profile,
			})
			require.NoError(t, err)
			assert.Equal(t, test.want, message)
		})
	}
}

func TestFormatMessageEmptyStatistics(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		phase string
		want  string
	}{
		{
			phase: "running",
			want: `🔄 rclone · copy — in progress

Transferred: 0 B
Transfers: 0
Checked: 0
Speed: 0 B/s
ETA: unknown

Elapsed: 0s`,
		},
		{
			phase: "end",
			want: `✅ rclone · copy — completed successfully

Transferred: 0 B
Transfers: 0
Checked: 0
Average speed: unknown
Max speed (observed): unknown

Duration: 0s
Finished: unknown`,
		},
	} {
		t.Run(test.phase, func(t *testing.T) {
			message, err := FormatMessage(State{Operation: "copy", Phase: test.phase})
			require.NoError(t, err)
			assert.Equal(t, test.want, message)
		})
	}
}

func TestFormatMessageEstimates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		totalBytes     int64
		totalTransfers int64
		eta            time.Duration
		etaKnown       bool
		wantBytes      string
		wantTransfers  string
		wantETA        string
	}{
		{"unknown", 0, 0, time.Minute, false, "Transferred: 1 KiB", "Transfers: 1", "ETA: unknown"},
		{"negative", -1, -1, -time.Second, true, "Transferred: 1 KiB", "Transfers: 1", "ETA: unknown"},
		{"only bytes known", 2048, 0, 0, true, "Transferred: 1 KiB / ≈2 KiB", "Transfers: 1", "ETA: ≈0s"},
		{"only transfers known", 0, 2, 999 * time.Millisecond, true, "Transferred: 1 KiB", "Transfers: 1 / 2", "ETA: ≈0s"},
		{"fractional seconds", 2048, 2, 1999 * time.Millisecond, true, "Transferred: 1 KiB / ≈2 KiB", "Transfers: 1 / 2", "ETA: ≈1s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, err := FormatMessage(State{
				Operation:      "copy",
				Phase:          "running",
				Bytes:          1024,
				Transfers:      1,
				TotalBytes:     test.totalBytes,
				TotalTransfers: test.totalTransfers,
				ETA:            test.eta,
				ETAKnown:       test.etaKnown,
			})
			require.NoError(t, err)
			lines := strings.Split(message, "\n")
			assert.Contains(t, lines, test.wantBytes)
			assert.Contains(t, lines, test.wantTransfers)
			assert.Contains(t, lines, test.wantETA)
		})
	}
}

func TestFormatMessageDeletes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		deletes     int64
		deletedDirs int64
		want        string
	}{
		{"files", 2, 0, "Deleted: 2 files, 0 directories"},
		{"directories", 0, 3, "Deleted: 0 files, 3 directories"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, err := FormatMessage(State{
				Phase:       "running",
				Deletes:     test.deletes,
				DeletedDirs: test.deletedDirs,
			})
			require.NoError(t, err)
			assert.Contains(t, strings.Split(message, "\n"), test.want)
		})
	}
}

func TestFormatMessageFinalSpeeds(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		elapsed       time.Duration
		maxSpeedKnown bool
		wantAverage   string
		wantMax       string
	}{
		{"fractional duration", 1500 * time.Millisecond, true, "Average speed: 1 KiB/s", "Max speed (observed): 0 B/s"},
		{"unknown duration", 0, false, "Average speed: unknown", "Max speed (observed): unknown"},
		{"negative duration", -time.Second, false, "Average speed: unknown", "Max speed (observed): unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, err := FormatMessage(State{
				Phase:         "end",
				Bytes:         1536,
				Speed:         9999,
				Elapsed:       test.elapsed,
				MaxSpeedKnown: test.maxSpeedKnown,
			})
			require.NoError(t, err)
			lines := strings.Split(message, "\n")
			assert.Contains(t, lines, test.wantAverage)
			assert.Contains(t, lines, test.wantMax)
		})
	}
}

func TestFormatMessageFinalOutcome(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		bytes      int64
		speed      float64
		err        error
		wantStatus string
	}{
		{"success despite incomplete estimate and zero speed", 1, 0, nil, "✅ rclone · copy — completed successfully"},
		{"failure despite complete estimate and positive speed", 2, 1024, errors.New("final check failed"), "❌ rclone · copy — failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, err := FormatMessage(State{
				Operation:  "copy",
				Phase:      "end",
				Bytes:      test.bytes,
				TotalBytes: 2,
				Speed:      test.speed,
				Error:      test.err,
			})
			require.NoError(t, err)
			assert.Equal(t, test.wantStatus, strings.Split(message, "\n")[0])
		})
	}
}

func TestFormatMessageInvalidPhase(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"", "finished", "RUNNING"} {
		t.Run(phase, func(t *testing.T) {
			message, err := FormatMessage(State{Phase: phase})
			require.EqualError(t, err, "unknown state: "+phase)
			assert.Empty(t, message)
		})
	}
}

func TestFormatTime(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value time.Time
		want  string
	}{
		{"unknown", time.Time{}, "unknown"},
		{"UTC", time.Date(2026, 9, 28, 1, 2, 3, 999999999, time.UTC), "2026-09-28 01:02:03 +00:00"},
		{"negative offset", time.Date(2026, 9, 28, 1, 2, 3, 0, time.FixedZone("UTC-0330", -3*60*60-30*60)), "2026-09-28 01:02:03 -03:30"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, formatTime(test.value))
		})
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value time.Duration
		want  string
	}{
		{0, "0s"},
		{999 * time.Millisecond, "0s"},
		{1999 * time.Millisecond, "1s"},
		{25*time.Hour + 2*time.Minute + 3*time.Second + 900*time.Millisecond, "1d1h2m3s"},
	} {
		t.Run(test.value.String(), func(t *testing.T) {
			assert.Equal(t, test.want, formatDuration(test.value))
		})
	}
}
