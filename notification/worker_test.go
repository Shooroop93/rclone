package notification

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type workerNotifierFunc func(context.Context, State) error

func (f workerNotifierFunc) Notify(ctx context.Context, state State) error {
	return f(ctx, state)
}

func TestWorkerCoalescesProgress(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		var got []State
		w := newWorker(workerNotifierFunc(func(ctx context.Context, state State) error {
			got = append(got, state)
			if state.Phase == "start" {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}))
		initial := State{Phase: "start", Operation: "copy"}
		first := State{Phase: "running", Bytes: 1}
		latest := State{Phase: "running", Bytes: 3}
		final := State{Phase: "end", Bytes: 3}
		go w.run(ctx, initial)
		synctest.Wait()

		require.True(t, w.trySend(first))
		require.False(t, w.trySend(State{Phase: "running", Bytes: 2}))
		require.True(t, w.updateProgress(latest))
		assert.Equal(t, []State{initial}, got)
		close(release)
		synctest.Wait()
		assert.Equal(t, []State{initial, latest}, got)

		require.True(t, w.trySendFinal(final))
		require.NoError(t, w.wait(context.Background()))
		assert.Equal(t, []State{initial, latest, final}, got)
	})
}

func TestWorkerFinalWithPendingProgress(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		var got []State
		w := newWorker(workerNotifierFunc(func(ctx context.Context, state State) error {
			got = append(got, state)
			if state.Phase == "start" {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}))
		initial := State{Phase: "start"}
		progress := State{Phase: "running", Bytes: 10}
		final := State{Phase: "end", Bytes: 20}
		go w.run(ctx, initial)
		synctest.Wait()

		require.True(t, w.updateProgress(progress))
		require.True(t, w.trySendFinal(final))
		require.False(t, w.trySendFinal(State{Phase: "end", Bytes: 99}))
		close(release)
		require.NoError(t, w.wait(context.Background()))

		assert.Equal(t, []State{initial, final}, got)
		w.updateProgress(State{Phase: "running", Bytes: 30})
		synctest.Wait()
		assert.Equal(t, []State{initial, final}, got)
	})
}

func TestWorkerStops(t *testing.T) {
	t.Parallel()

	for _, stop := range []string{"cancel", "close updates", "close final"} {
		t.Run(stop, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var got []State
				w := newWorker(workerNotifierFunc(func(_ context.Context, state State) error {
					got = append(got, state)
					return nil
				}))
				initial := State{Phase: "start"}
				go w.run(ctx, initial)
				synctest.Wait()
				switch stop {
				case "cancel":
					cancel()
				case "close updates":
					close(w.updates)
				case "close final":
					close(w.final)
				}
				require.NoError(t, w.wait(context.Background()))
				assert.Equal(t, []State{initial}, got)
				if stop == "cancel" {
					assert.ErrorIs(t, w.err, context.Canceled)
				} else {
					assert.NoError(t, w.err)
				}
			})
		})
	}
}

func TestWorkerNotifyTimeout(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name           string
		requestTimeout time.Duration
		parentTimeout  time.Duration
		wantElapsed    time.Duration
	}{
		{name: "default", wantElapsed: 10 * time.Second},
		{name: "custom", requestTimeout: 2 * time.Second, wantElapsed: 2 * time.Second},
		{name: "parent deadline", parentTimeout: time.Second, wantElapsed: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				if test.parentTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, test.parentTimeout)
					defer cancel()
				}
				initial := State{Phase: "start", Operation: "copy"}
				var requestCtx context.Context
				w := newWorker(workerNotifierFunc(func(ctx context.Context, state State) error {
					requestCtx = ctx
					assert.Equal(t, initial, state)
					<-ctx.Done()
					return ctx.Err()
				}))
				if test.requestTimeout > 0 {
					w.timeout = test.requestTimeout
				}

				started := time.Now()
				require.ErrorIs(t, w.notify(ctx, initial), context.DeadlineExceeded)
				assert.Equal(t, test.wantElapsed, time.Since(started))
				assert.ErrorIs(t, requestCtx.Err(), context.DeadlineExceeded)
			})
		})
	}
}

func TestWorkerContinuesAfterNotificationErrors(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var got []State
		var contexts []context.Context
		w := newWorker(workerNotifierFunc(func(ctx context.Context, state State) error {
			got = append(got, state)
			contexts = append(contexts, ctx)
			if state.Phase != "end" {
				return errors.New("notification unavailable")
			}
			return nil
		}))
		initial := State{Phase: "start"}
		progress := State{Phase: "running", Bytes: 10}
		final := State{Phase: "end", Bytes: 20}
		go w.run(ctx, initial)
		synctest.Wait()
		require.True(t, w.updateProgress(progress))
		synctest.Wait()
		require.True(t, w.trySendFinal(final))
		require.NoError(t, w.wait(context.Background()))

		assert.Equal(t, []State{initial, progress, final}, got)
		assert.NoError(t, w.err)
		for _, requestCtx := range contexts {
			assert.ErrorIs(t, requestCtx.Err(), context.Canceled)
		}
		assert.NoError(t, ctx.Err())
	})
}

func TestWorkerFinalRetries(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		succeedOn int
		attempts  int
	}{
		{name: "immediate success", succeedOn: 1, attempts: 1},
		{name: "second attempt", succeedOn: 2, attempts: 2},
		{name: "third attempt", succeedOn: 3, attempts: 3},
		{name: "exhausted", attempts: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("notification unavailable")
				final := State{Phase: "end", Bytes: 20, Error: errors.New("operation failed")}
				var got []State
				var attempts []time.Time
				w := newWorker(workerNotifierFunc(func(_ context.Context, state State) error {
					if state.Phase != "end" {
						return nil
					}
					got = append(got, state)
					attempts = append(attempts, time.Now())
					if len(attempts) == test.succeedOn {
						return nil
					}
					return failure
				}))
				go w.run(ctx, State{Phase: "start"})
				synctest.Wait()
				started := time.Now()
				require.True(t, w.trySendFinal(final))
				require.NoError(t, w.wait(context.Background()))

				require.Len(t, attempts, test.attempts)
				for i, attemptedAt := range attempts {
					assert.Equal(t, time.Duration(i)*time.Second, attemptedAt.Sub(started))
					assert.Equal(t, final, got[i])
				}
				assert.Equal(t, time.Duration(test.attempts-1)*time.Second, time.Since(started))
				if test.succeedOn > 0 {
					assert.NoError(t, w.err)
				} else {
					assert.ErrorIs(t, w.err, failure)
				}
			})
		})
	}
}

func TestWorkerFinalRetryCancellation(t *testing.T) {
	t.Parallel()

	for _, duringRequest := range []bool{false, true} {
		name := "during retry delay"
		if duringRequest {
			name = "during request"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				attempts := 0
				w := newWorker(workerNotifierFunc(func(ctx context.Context, state State) error {
					if state.Phase != "end" {
						return nil
					}
					attempts++
					if duringRequest {
						<-ctx.Done()
						return ctx.Err()
					}
					return errors.New("notification unavailable")
				}))
				go w.run(ctx, State{Phase: "start"})
				synctest.Wait()
				require.True(t, w.trySendFinal(State{Phase: "end"}))
				synctest.Wait()
				require.Equal(t, 1, attempts)
				time.Sleep(500 * time.Millisecond)
				canceledAt := time.Now()
				cancel()
				require.NoError(t, w.wait(context.Background()))

				assert.Zero(t, time.Since(canceledAt), "cancellation must interrupt the retry delay")
				assert.Equal(t, 1, attempts)
				assert.ErrorIs(t, w.err, context.Canceled)
			})
		})
	}
}

func TestWorkerFinalAlreadyCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWorker(workerNotifierFunc(func(context.Context, State) error {
		t.Error("a canceled final delivery must not call the notifier")
		return nil
	}))
	w.finishDelivery(ctx, State{Phase: "end"})
	assert.ErrorIs(t, w.err, context.Canceled)
}
