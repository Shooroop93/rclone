package notification

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerTimingOptions(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		options  *Options
		interval time.Duration
		timeout  time.Duration
	}{
		{"default constructor", nil, 5 * time.Second, 10 * time.Second},
		{"zero", &Options{}, 5 * time.Second, 10 * time.Second},
		{"negative", &Options{Interval: -time.Second, RequestTimeout: -time.Second, ShutdownTimeout: -time.Second}, 5 * time.Second, 10 * time.Second},
		{"custom", &Options{Interval: 3 * time.Second, RequestTimeout: 250 * time.Millisecond}, 3 * time.Second, 250 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				ctx, _ := newSnapshotStats(t)
				var phases []string
				var sentAt []time.Time
				notifiers := []Notifier{workerNotifierFunc(func(ctx context.Context, state State) error {
					deadline, ok := ctx.Deadline()
					assert.True(t, ok, "each request must have a deadline")
					assert.Equal(t, test.timeout, deadline.Sub(time.Now()))
					phases = append(phases, state.Phase)
					sentAt = append(sentAt, time.Now())
					return nil
				})}
				var m *Manager
				if test.options == nil {
					m = NewManager(notifiers)
				} else {
					m = NewManagerWithOptions(notifiers, *test.options)
				}
				started := time.Now()
				m.Start(ctx, State{Phase: "start", StartedAt: started})
				defer func() { _ = m.Finish(context.Background(), nil) }()
				synctest.Wait()
				require.Equal(t, []string{"start"}, phases)

				time.Sleep(test.interval - time.Nanosecond)
				synctest.Wait()
				assert.Equal(t, []string{"start"}, phases)
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				require.Equal(t, []string{"start", "running"}, phases)
				assert.Equal(t, started.Add(test.interval), sentAt[1])

				require.NoError(t, m.Finish(context.Background(), nil))
				assert.Equal(t, []string{"start", "running", "end"}, phases)
			})
		})
	}
}
