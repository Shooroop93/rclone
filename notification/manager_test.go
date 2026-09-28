package notification

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rclone/rclone/fs/accounting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerLifecycle(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stats := newSnapshotStats(t)
		var got [2][]State
		notifiers := make([]Notifier, len(got))
		for i := range notifiers {
			notifiers[i] = workerNotifierFunc(func(_ context.Context, state State) error {
				got[i] = append(got[i], state)
				return nil
			})
		}
		m := NewManagerWithOptions(notifiers, Options{Interval: 2 * time.Second})
		initial := State{
			Phase: "start", Operation: "sync", Source: "source:path", Destination: "dest:path",
			Profile: "team", StartedAt: time.Now(),
		}
		m.Start(ctx, initial)
		defer m.cancel()
		synctest.Wait()
		m.Start(ctx, State{Phase: "start", Operation: "ignored second start"})
		synctest.Wait()
		for i := range got {
			require.Equal(t, []State{initial}, got[i])
		}

		stats.BytesNoNetwork(100)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		progress := initial
		progress.Phase = "running"
		progress.Bytes, progress.TotalBytes = 100, 100
		progress.MaxSpeedKnown = true
		progress.Elapsed = 2 * time.Second
		for i := range got {
			require.Equal(t, []State{initial, progress}, got[i])
		}

		stats.BytesNoNetwork(50)
		_ = stats.Error(errors.New("previous operation attempt failed"))
		time.Sleep(500 * time.Millisecond)
		finishedAt := time.Now()
		require.NoError(t, m.Finish(context.Background(), nil))
		final := progress
		final.Phase = "end"
		final.Bytes, final.TotalBytes = 150, 150
		final.FinishedAt = finishedAt
		final.Elapsed = 2500 * time.Millisecond
		for i := range got {
			assert.Equal(t, []State{initial, progress, final}, got[i])
		}
		assert.Equal(t, int64(1), stats.GetErrors())

		require.NoError(t, m.Finish(context.Background(), errors.New("ignored second result")))
		m.Start(ctx, initial)
		time.Sleep(4 * time.Second)
		synctest.Wait()
		for i := range got {
			assert.Equal(t, []State{initial, progress, final}, got[i])
		}
	})
}

func TestManagerPreservesObservedMaxSpeed(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stats := newSnapshotStats(t)
		transfer := stats.NewTransferRemoteSize("active", 0, nil, nil)
		defer transfer.Done(ctx, nil)
		var got []State
		m := NewManagerWithOptions([]Notifier{workerNotifierFunc(func(_ context.Context, state State) error {
			got = append(got, state)
			return nil
		})}, Options{Interval: 1250 * time.Millisecond})
		m.Start(ctx, State{Phase: "start", StartedAt: time.Now()})
		defer m.cancel()
		synctest.Wait()

		// Sample between accounting ticks so both tickers cannot race at the same instant.
		for _, size := range []int64{1000, 3000, 0} {
			stats.Bytes(size)
			time.Sleep(1250 * time.Millisecond)
			synctest.Wait()
		}
		require.NoError(t, m.Finish(context.Background(), nil))
		require.Len(t, got, 5)
		assert.Equal(t, float64(1000), got[1].MaxSpeed)
		assert.Equal(t, float64(2000), got[2].MaxSpeed)
		assert.Less(t, got[3].Speed, got[2].Speed)
		for _, state := range got[2:] {
			assert.True(t, state.MaxSpeedKnown)
			assert.Equal(t, float64(2000), state.MaxSpeed)
		}
	})
}

func TestManagerOperationCancellation(t *testing.T) {
	t.Parallel()

	for _, deadline := range []bool{false, true} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx, stats := newSnapshotStats(t)
				type contextKey struct{}
				ctx = context.WithValue(ctx, contextKey{}, "operation value")
				var cancel context.CancelFunc
				if deadline {
					ctx, cancel = context.WithTimeout(ctx, time.Second)
				} else {
					ctx, cancel = context.WithCancel(ctx)
				}
				defer cancel()
				var got []State
				m := NewManager([]Notifier{workerNotifierFunc(func(ctx context.Context, state State) error {
					assert.NoError(t, ctx.Err(), "delivery must survive operation cancellation")
					assert.Equal(t, "operation value", ctx.Value(contextKey{}))
					got = append(got, state)
					return nil
				})})
				initial := State{Phase: "start", StartedAt: time.Now()}
				m.Start(ctx, initial)
				defer m.cancel()
				synctest.Wait()
				stats.BytesNoNetwork(123)
				if deadline {
					time.Sleep(time.Second)
				} else {
					cancel()
				}
				synctest.Wait()
				require.NoError(t, m.Finish(context.Background(), nil))
				require.Len(t, got, 2)
				assert.Equal(t, initial, got[0])
				assert.Equal(t, "end", got[1].Phase)
				assert.ErrorIs(t, got[1].Error, ctx.Err())
				assert.Equal(t, int64(123), got[1].Bytes)
				assert.Equal(t, time.Now(), got[1].FinishedAt)
			})
		})
	}
}

func TestManagerFinalWithPendingProgress(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, stats := newSnapshotStats(t)
		release := make(chan struct{})
		var got []State
		attempts := 0
		m := NewManagerWithOptions([]Notifier{workerNotifierFunc(func(ctx context.Context, state State) error {
			got = append(got, state)
			if state.Phase == "start" {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if state.Phase == "end" {
				attempts++
				if attempts == 1 {
					return errors.New("temporary delivery failure")
				}
			}
			return nil
		})}, Options{Interval: time.Second})
		initial := State{Phase: "start", StartedAt: time.Now()}
		m.Start(ctx, initial)
		defer m.cancel()
		synctest.Wait()
		for range 3 {
			stats.BytesNoNetwork(10)
			time.Sleep(time.Second)
			synctest.Wait()
		}
		assert.Equal(t, []State{initial}, got)
		operationErr := errors.New("operation failed")
		finishedAt := time.Now()
		done := make(chan error, 1)
		go func() { done <- m.Finish(context.Background(), operationErr) }()
		synctest.Wait()
		time.Sleep(time.Second)
		close(release)
		require.NoError(t, <-done)
		require.Len(t, got, 3)
		assert.Equal(t, "end", got[1].Phase, "queued progress must not delay the final state")
		assert.Equal(t, int64(30), got[1].Bytes)
		assert.ErrorIs(t, got[1].Error, operationErr)
		assert.Equal(t, finishedAt, got[1].FinishedAt)
		assert.Equal(t, 3*time.Second, got[1].Elapsed, "delivery wait is excluded from operation duration")
		assert.Equal(t, got[1], got[2], "a retry must deliver the same final snapshot")
		assert.Equal(t, 2*time.Second, time.Since(finishedAt))
		time.Sleep(2 * time.Second)
		synctest.Wait()
		assert.Len(t, got, 3)
	})
}

func TestManagerConcurrentFinish(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, _ := newSnapshotStats(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		release := make(chan struct{})
		var finals []State
		m := NewManager([]Notifier{workerNotifierFunc(func(ctx context.Context, state State) error {
			if state.Phase == "end" {
				finals = append(finals, state)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})})
		m.Start(ctx, State{Phase: "start", StartedAt: time.Now()})
		defer m.cancel()
		firstResult := errors.New("first operation result")
		firstDone := make(chan error, 1)
		go func() { firstDone <- m.Finish(context.Background(), firstResult) }()
		synctest.Wait()
		cancel()
		waitCtx, stopWaiting := context.WithTimeout(context.Background(), time.Second)
		defer stopWaiting()
		require.ErrorIs(t, m.Finish(waitCtx, errors.New("ignored second result")), context.DeadlineExceeded)
		select {
		case err := <-firstDone:
			t.Fatalf("a second waiter's timeout stopped final delivery: %v", err)
		default:
		}
		close(release)
		require.NoError(t, <-firstDone)
		require.NoError(t, m.Finish(context.Background(), nil))
		require.Len(t, finals, 1)
		assert.ErrorIs(t, finals[0].Error, firstResult)
	})
}

func TestManagerFinishDeadline(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		shutdown time.Duration
		caller   time.Duration
		want     time.Duration
	}{
		{name: "default shutdown", want: 15 * time.Second},
		{name: "custom shutdown", shutdown: 2 * time.Second, want: 2 * time.Second},
		{name: "caller deadline", shutdown: 10 * time.Second, caller: time.Second, want: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx, _ := newSnapshotStats(t)
				var requests [2]context.Context
				notifiers := make([]Notifier, len(requests))
				for i := range notifiers {
					notifiers[i] = workerNotifierFunc(func(ctx context.Context, state State) error {
						if state.Phase == "end" {
							requests[i] = ctx
							<-ctx.Done()
							return ctx.Err()
						}
						return nil
					})
				}
				m := NewManagerWithOptions(notifiers, Options{RequestTimeout: time.Minute, ShutdownTimeout: test.shutdown})
				m.Start(ctx, State{Phase: "start", StartedAt: time.Now()})
				defer m.cancel()
				waitCtx := context.Background()
				if test.caller > 0 {
					var cancel context.CancelFunc
					waitCtx, cancel = context.WithTimeout(waitCtx, test.caller)
					defer cancel()
				}
				started := time.Now()
				require.ErrorIs(t, m.Finish(waitCtx, nil), context.DeadlineExceeded)
				assert.Equal(t, test.want, time.Since(started), "providers must share one shutdown deadline")
				synctest.Wait()
				for i, request := range requests {
					require.NotNil(t, request)
					assert.ErrorIs(t, request.Err(), context.Canceled)
					select {
					case <-m.workers[i].done:
					default:
						t.Error("worker did not stop after shutdown")
					}
				}
			})
		})
	}
}

func TestManagerFinishCanceled(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, _ := newSnapshotStats(t)
		var requestCtx context.Context
		m := NewManager([]Notifier{workerNotifierFunc(func(ctx context.Context, _ State) error {
			requestCtx = ctx
			<-ctx.Done()
			return ctx.Err()
		})})
		m.Start(ctx, State{Phase: "start", StartedAt: time.Now()})
		defer m.cancel()
		synctest.Wait()
		waitCtx, cancel := context.WithCancel(context.Background())
		cancel()
		started := time.Now()
		require.ErrorIs(t, m.Finish(waitCtx, nil), context.Canceled)
		synctest.Wait()
		assert.Zero(t, time.Since(started))
		assert.ErrorIs(t, requestCtx.Err(), context.Canceled)
	})
}

func TestManagerFinishBoundedWithUncooperativeNotifier(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, _ := newSnapshotStats(t)
		release := make(chan struct{})
		defer close(release)
		m := NewManagerWithOptions([]Notifier{workerNotifierFunc(func(ctx context.Context, _ State) error {
			<-release
			return ctx.Err()
		})}, Options{ShutdownTimeout: 2 * time.Second, RequestTimeout: time.Second})
		m.Start(ctx, State{Phase: "start", StartedAt: time.Now()})
		defer m.cancel()
		synctest.Wait()
		started := time.Now()
		require.ErrorIs(t, m.Finish(context.Background(), nil), context.DeadlineExceeded)
		assert.Equal(t, 2*time.Second, time.Since(started))
	})
}

func TestManagerNotifierIsolation(t *testing.T) {
	// Keep this test sequential because it checks the global error count.
	synctest.Test(t, func(t *testing.T) {
		ctx, stats := newSnapshotStats(t)
		globalErrors := accounting.GlobalStats().GetErrors()
		release := make(chan struct{})
		var slow, healthy []State
		firstFailure := errors.New("first provider unavailable")
		secondFailure := errors.New("second provider unavailable")
		m := NewManagerWithOptions([]Notifier{
			workerNotifierFunc(func(ctx context.Context, state State) error {
				slow = append(slow, state)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}),
			workerNotifierFunc(func(context.Context, State) error { return firstFailure }),
			workerNotifierFunc(func(context.Context, State) error { return secondFailure }),
			workerNotifierFunc(func(_ context.Context, state State) error {
				healthy = append(healthy, state)
				return nil
			}),
		}, Options{Interval: time.Second})
		initial := State{Phase: "start", StartedAt: time.Now()}
		m.Start(ctx, initial)
		defer m.cancel()
		synctest.Wait()
		stats.BytesNoNetwork(42)
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, []State{initial}, slow)
		require.Len(t, healthy, 2)
		assert.Equal(t, "running", healthy[1].Phase)
		assert.Equal(t, int64(42), healthy[1].Bytes)

		done := make(chan error, 1)
		go func() { done <- m.Finish(context.Background(), nil) }()
		synctest.Wait()
		require.Len(t, healthy, 3)
		assert.Equal(t, "end", healthy[2].Phase)
		assert.NoError(t, healthy[2].Error)
		assert.Equal(t, []State{initial}, slow, "a blocked provider must not delay another provider's final delivery")
		close(release)
		err := <-done
		assert.ErrorIs(t, err, firstFailure)
		assert.ErrorIs(t, err, secondFailure)
		require.Len(t, slow, 2)
		assert.Equal(t, healthy[2], slow[1])
		assert.Zero(t, stats.GetErrors())
		assert.Equal(t, globalErrors, accounting.GlobalStats().GetErrors())
	})
}

func TestManagerFinishBeforeStart(t *testing.T) {
	t.Parallel()

	m := NewManager(nil)
	require.ErrorIs(t, m.Finish(context.Background(), nil), ErrNotStarted)
	m.Start(context.Background(), State{Phase: "start"})
	require.NoError(t, m.Finish(context.Background(), nil))
}

func TestManagerWithoutNotifiers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil)
		m.Start(context.Background(), State{Phase: "start"})
		started := time.Now()
		require.NoError(t, m.Finish(context.Background(), errors.New("operation failed")))
		require.NoError(t, m.Finish(context.Background(), nil))
		assert.Zero(t, time.Since(started))
	})
}
