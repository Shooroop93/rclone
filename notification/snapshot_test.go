package notification

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSnapshotStats(t *testing.T) (context.Context, *accounting.StatsInfo) {
	t.Helper()
	ctx, config := fs.AddConfig(context.Background())
	config.MaxDelete = -1
	config.MaxDeleteSize = -1
	group := "notification/" + t.Name()
	ctx = accounting.WithStatsGroup(ctx, group)
	stats := accounting.Stats(ctx)
	t.Cleanup(func() {
		_, err := rc.Calls.Get("core/stats-delete").Fn(ctx, rc.Params{"group": group})
		assert.NoError(t, err)
	})
	return ctx, stats
}

func TestSnapshotStateCounters(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stats := newSnapshotStats(t)
		stats.BytesNoNetwork(1024)
		for range 2 {
			stats.NewTransferRemoteSize("completed", 0, nil, nil).Done(ctx, nil)
		}
		for range 3 {
			stats.NewCheckingTransfer(mockobject.New("checked"), "checking").Done(ctx, nil)
		}
		for range 4 {
			require.NoError(t, stats.DeleteFile(ctx, 10))
		}
		stats.DeletedDirs(5)
		stats.SetTransferQueue(7, 8192)
		active := stats.NewTransferRemoteSize("active", 2048, nil, nil)
		t.Cleanup(func() { active.Done(ctx, nil) })
		account := active.Account(ctx, io.NopCloser(bytes.NewReader(make([]byte, 2048))))
		_, err := io.ReadFull(account, make([]byte, 512))
		require.NoError(t, err)

		input := State{
			Operation: "sync", Phase: "running", Source: "source:path", Destination: "dest:path", Profile: "team",
			StartedAt: time.Now().Add(-10 * time.Second), MaxSpeed: 32768, MaxSpeedKnown: true,
			ETA: time.Minute, ETAKnown: true,
		}
		original := input
		got, err := snapshotState(ctx, input)
		require.NoError(t, err)
		assert.Equal(t, State{
			Operation: "sync", Phase: "running", Source: "source:path", Destination: "dest:path", Profile: "team",
			Bytes: 1536, TotalBytes: 11264, Transfers: 2, TotalTransfers: 10,
			Checks: 3, Deletes: 4, DeletedDirs: 5,
			MaxSpeed: 32768, MaxSpeedKnown: true,
			StartedAt: input.StartedAt, Elapsed: 10 * time.Second,
		}, got)
		assert.Equal(t, original, input, "sampling must not mutate the caller's snapshot")

		stats.SetTransferQueue(1, 1024)
		next, err := snapshotState(ctx, got)
		require.NoError(t, err)
		assert.Equal(t, int64(4096), next.TotalBytes)
		assert.Equal(t, int64(4), next.TotalTransfers)
		assert.Equal(t, int64(11264), got.TotalBytes, "queued snapshots must retain their old values")
	})
}

func TestSnapshotStateSpeedAndETA(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stats := newSnapshotStats(t)
		transfer := stats.NewTransferRemoteSize("active", 0, nil, nil)
		defer transfer.Done(ctx, nil)
		// Let accounting create its ticker before advancing the fake clock.
		synctest.Wait()
		state, err := snapshotState(ctx, State{StartedAt: time.Now(), Phase: "running", MaxSpeed: 9999})
		require.NoError(t, err)
		assert.Zero(t, state.Speed)
		assert.Zero(t, state.MaxSpeed, "an unknown previous maximum must not override the first observation")
		assert.True(t, state.MaxSpeedKnown)
		assert.False(t, state.ETAKnown)

		stats.SetTransferQueue(1, 3000)
		stats.Bytes(1000)
		time.Sleep(time.Second)
		synctest.Wait()
		state, err = snapshotState(ctx, state)
		require.NoError(t, err)
		assert.Equal(t, float64(1000), state.Speed)
		assert.Equal(t, float64(1000), state.MaxSpeed)
		assert.Equal(t, 3*time.Second, state.ETA)
		assert.True(t, state.ETAKnown)

		stats.Bytes(3000)
		stats.SetTransferQueue(1, 6000)
		time.Sleep(time.Second)
		synctest.Wait()
		state, err = snapshotState(ctx, state)
		require.NoError(t, err)
		assert.Equal(t, float64(2000), state.Speed)
		assert.Equal(t, float64(2000), state.MaxSpeed)
		assert.Equal(t, 3*time.Second, state.ETA)
		assert.Equal(t, 2*time.Second, state.Elapsed)

		time.Sleep(time.Second)
		synctest.Wait()
		state, err = snapshotState(ctx, state)
		require.NoError(t, err)
		assert.Less(t, state.Speed, state.MaxSpeed)
		assert.Equal(t, float64(2000), state.MaxSpeed)

		stats.SetTransferQueue(0, 0)
		state, err = snapshotState(ctx, state)
		require.NoError(t, err)
		assert.True(t, state.ETAKnown, "zero seconds remaining is a known estimate")
		assert.Zero(t, state.ETA)
	})
}

func TestSnapshotStateLargeETA(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stats := newSnapshotStats(t)
		transfer := stats.NewTransferRemoteSize("active", 0, nil, nil)
		defer transfer.Done(ctx, nil)
		synctest.Wait()
		stats.SetTransferQueue(1, math.MaxInt64-1)
		stats.Bytes(1)
		time.Sleep(time.Second)
		synctest.Wait()

		state, err := snapshotState(ctx, State{})
		require.NoError(t, err)
		assert.True(t, state.ETAKnown)
		assert.Equal(t, time.Duration(9223372036)*time.Second, state.ETA)
	})
}

func TestSnapshotStateElapsed(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, _ := newSnapshotStats(t)
		now := time.Now()
		for _, test := range []struct {
			name     string
			started  time.Time
			finished time.Time
			want     time.Duration
		}{
			{"no start", time.Time{}, time.Time{}, 0},
			{"finish without start", time.Time{}, now, 0},
			{"running", now.Add(-5 * time.Second), time.Time{}, 5 * time.Second},
			{"finished", now.Add(-10 * time.Second), now.Add(-2 * time.Second), 8 * time.Second},
			{"future start", now.Add(time.Second), time.Time{}, 0},
			{"finish before start", now, now.Add(-time.Second), 0},
		} {
			state, err := snapshotState(ctx, State{StartedAt: test.started, FinishedAt: test.finished, Elapsed: time.Hour})
			require.NoError(t, err, test.name)
			assert.Equal(t, test.want, state.Elapsed, test.name)
			assert.Equal(t, test.started, state.StartedAt, test.name)
			assert.Equal(t, test.finished, state.FinishedAt, test.name)
		}
	})
}

func TestSnapshotStateFinalError(t *testing.T) {
	t.Parallel()

	ctx, stats := newSnapshotStats(t)
	previousAttempt := errors.New("previous attempt failed")
	_ = stats.Error(previousAttempt)
	commandError := errors.New("final command result")
	for _, finalError := range []error{nil, commandError} {
		state, err := snapshotState(ctx, State{Phase: "end", Error: finalError})
		require.NoError(t, err)
		assert.Equal(t, finalError, state.Error)
		assert.Equal(t, "end", state.Phase)
		assert.Equal(t, int64(1), stats.GetErrors(), "sampling must not alter operation errors")
	}
}
