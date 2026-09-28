package notification

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
)

// Notifier delivers operation snapshots to a notification provider.
type Notifier interface {
	// Notify delivers state and must stop promptly when ctx is canceled.
	Notify(ctx context.Context, state State) error
}

// ErrNotStarted is returned by Finish when Start has not been called.
var ErrNotStarted = errors.New("notification manager has not been started")

// Options controls notification timing. Nonpositive values use the defaults.
type Options struct {
	// Interval is the time between progress snapshots; the default is 5 seconds.
	Interval time.Duration
	// RequestTimeout limits each Notify call; the default is 10 seconds.
	RequestTimeout time.Duration
	// ShutdownTimeout limits final delivery for all providers; the default is 15 seconds.
	ShutdownTimeout time.Duration
}

// State is a snapshot of an operation for notification providers.
type State struct {
	// Operation is the command name.
	Operation string
	// Phase is start, running, or end.
	Phase string
	// Source is the source path displayed in the notification.
	Source string
	// Destination is the destination path displayed in the notification.
	Destination string
	// Profile is the caller-provided profile label.
	Profile string

	// Bytes is the number of bytes recorded as transferred by accounting.
	Bytes int64
	// TotalBytes is the current estimated transfer size; nonpositive values are unknown.
	TotalBytes int64
	// Transfers is the number of completed file transfers.
	Transfers int64
	// TotalTransfers is the current total of completed, active, and queued transfers.
	TotalTransfers int64
	// Checks is the number of completed checks.
	Checks int64
	// Deletes is the number of deleted files.
	Deletes int64
	// DeletedDirs is the number of deleted directories.
	DeletedDirs int64
	// Speed is the transfer rate in bytes per second.
	Speed float64
	// MaxSpeed is the highest observed transfer rate in bytes per second.
	MaxSpeed float64
	// MaxSpeedKnown reports whether a transfer rate has been observed.
	MaxSpeedKnown bool
	// ETA is the estimated remaining duration when ETAKnown is true.
	ETA time.Duration
	// ETAKnown distinguishes an available estimate from an unknown one.
	ETAKnown bool

	// StartedAt is the operation start time, including all retry attempts.
	StartedAt time.Time
	// FinishedAt is the operation completion time, or zero while it is running.
	FinishedAt time.Time
	// Elapsed is the operation duration at the time of this snapshot.
	Elapsed time.Duration
	// Error is the final operation error; nil means success when Phase is end.
	Error error
}

// snapshotState returns state with current accounting statistics and elapsed time.
// The caller supplies operation metadata, start and finish times, and the final error.
// Pass the previous snapshot to preserve the highest observed speed.
// Elapsed is zero when StartedAt is unset; a zero FinishedAt uses the current time.
// On failure it returns a zero State and an error.
func snapshotState(ctx context.Context, state State) (State, error) {
	stats, err := accounting.Stats(ctx).RemoteStats(true)
	if err != nil {
		return State{}, fmt.Errorf("notification: read accounting statistics: %w", err)
	}

	state.Bytes, err = stats.GetInt64("bytes")
	if err != nil {
		return State{}, fmt.Errorf("notification: read bytes: %w", err)
	}
	state.TotalBytes, err = stats.GetInt64("totalBytes")
	if err != nil {
		return State{}, fmt.Errorf("notification: read total bytes: %w", err)
	}
	state.Transfers, err = stats.GetInt64("transfers")
	if err != nil {
		return State{}, fmt.Errorf("notification: read transfers: %w", err)
	}
	state.TotalTransfers, err = stats.GetInt64("totalTransfers")
	if err != nil {
		return State{}, fmt.Errorf("notification: read total transfers: %w", err)
	}
	state.Checks, err = stats.GetInt64("checks")
	if err != nil {
		return State{}, fmt.Errorf("notification: read checks: %w", err)
	}
	state.Deletes, err = stats.GetInt64("deletes")
	if err != nil {
		return State{}, fmt.Errorf("notification: read deletes: %w", err)
	}
	state.DeletedDirs, err = stats.GetInt64("deletedDirs")
	if err != nil {
		return State{}, fmt.Errorf("notification: read deleted directories: %w", err)
	}
	state.Speed, err = stats.GetFloat64("speed")
	if err != nil {
		return State{}, fmt.Errorf("notification: read speed: %w", err)
	}

	state.ETA = 0
	state.ETAKnown = false
	if stats["eta"] != nil {
		seconds, err := stats.GetFloat64("eta")
		if err != nil {
			return State{}, fmt.Errorf("notification: read ETA: %w", err)
		}
		// Accounting reports ETA in whole seconds.
		state.ETA = time.Duration(seconds) * time.Second
		state.ETAKnown = true
	}

	if !state.MaxSpeedKnown || state.Speed > state.MaxSpeed {
		state.MaxSpeed = state.Speed
	}
	state.MaxSpeedKnown = true

	state.Elapsed = 0
	if !state.StartedAt.IsZero() {
		finishedAt := state.FinishedAt
		if finishedAt.IsZero() {
			finishedAt = time.Now()
		}
		state.Elapsed = max(0, finishedAt.Sub(state.StartedAt))
	}

	return state, nil
}

type worker struct {
	notifier Notifier
	updates  chan State
	final    chan State
	done     chan struct{}
	timeout  time.Duration
	err      error
}

type finalRequest struct {
	ctx        context.Context
	err        error
	finishedAt time.Time
}

// Manager delivers notifications for a single operation, including its retries.
// Create it with NewManager or NewManagerWithOptions and call Start, then Finish.
type Manager struct {
	workers []*worker
	cancel  context.CancelFunc
	state   State
	options Options

	mu         sync.Mutex
	started    bool
	finishOnce sync.Once
	finish     chan finalRequest
	done       chan struct{}
	err        error
}

// NewManager creates a manager with the default timing options.
func NewManager(notifiers []Notifier) *Manager {
	return NewManagerWithOptions(notifiers, Options{})
}

// NewManagerWithOptions creates a manager with a worker for each notifier.
// Each notifier must be non-nil and belong only to this manager during the operation.
func NewManagerWithOptions(notifiers []Notifier, options Options) *Manager {
	if options.Interval <= 0 {
		options.Interval = 5 * time.Second
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 10 * time.Second
	}
	if options.ShutdownTimeout <= 0 {
		options.ShutdownTimeout = 15 * time.Second
	}
	m := &Manager{options: options}
	for _, notifier := range notifiers {
		w := newWorker(notifier)
		w.timeout = options.RequestTimeout
		m.workers = append(m.workers, w)
	}

	return m
}

func newWorker(notifier Notifier) *worker {
	return &worker{
		updates:  make(chan State, 1),
		final:    make(chan State, 1),
		notifier: notifier,
		done:     make(chan struct{}),
		timeout:  10 * time.Second,
	}
}

// Start starts asynchronous delivery of initial and periodic progress snapshots.
// The caller supplies initial.Phase as "start" and the operation's StartedAt.
// Canceling ctx initiates bounded final delivery with ctx.Err() as the result.
// Subsequent calls have no effect; a manager cannot be reused for another operation.
func (m *Manager) Start(ctx context.Context, initial State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return
	}
	m.started = true
	// Delivery must remain possible after the operation's context is canceled.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.cancel = cancel
	m.state = initial
	m.finish = make(chan finalRequest, 1)
	m.done = make(chan struct{})
	if len(m.workers) == 0 {
		cancel()
		close(m.done)
		return
	}

	for _, w := range m.workers {
		go w.run(runCtx, m.state)
	}
	go m.run(ctx, runCtx)
}

// Finish stops progress, sends the final operation result, and waits for delivery.
// The first finalization request, from Finish or cancellation, determines the result.
// Use a separate waiting context when the operation's context has been canceled.
// Waiting is limited by both ctx and Options.ShutdownTimeout. The returned error
// describes notification delivery, not the operation result; it must not be counted
// as an operation error. Finish returns ErrNotStarted if Start has not been called.
// Concurrent or repeated calls wait for the same final delivery.
// Failed final deliveries are retried up to twice, one second apart, within the limit.
func (m *Manager) Finish(ctx context.Context, result error) error {
	m.mu.Lock()
	started := m.started
	m.mu.Unlock()
	if !started {
		return ErrNotStarted
	}
	waitCtx, cancel := context.WithTimeout(ctx, m.options.ShutdownTimeout)
	defer cancel()
	first := m.requestFinal(waitCtx, result)
	select {
	case <-m.done:
		return m.err
	default:
	}
	select {
	case <-m.done:
		return m.err
	case <-waitCtx.Done():
		if first {
			m.cancel()
		}
		return waitCtx.Err()
	}
}

func (m *Manager) requestFinal(ctx context.Context, result error) (first bool) {
	m.finishOnce.Do(func() {
		first = true
		m.finish <- finalRequest{ctx: ctx, err: result, finishedAt: time.Now()}
	})
	return first
}

func (m *Manager) run(ctx, runCtx context.Context) {
	defer close(m.done)
	defer m.cancel()
	ticker := time.NewTicker(m.options.Interval)
	defer ticker.Stop()
	for {
		select {
		case request := <-m.finish:
			ticker.Stop()
			m.finishRun(ctx, request)
			return
		case <-ctx.Done():
			m.requestFinal(context.Background(), ctx.Err())
			ticker.Stop()
			m.finishRun(ctx, <-m.finish)
			return
		case <-runCtx.Done():
			m.err = runCtx.Err()
			return
		case <-ticker.C:
			m.publishProgress(ctx)
		}
	}
}

func (m *Manager) publishProgress(ctx context.Context) {
	state, err := snapshotState(ctx, m.state)
	if err != nil {
		fs.Logf(nil, "notification: failed to collect progress: %v", err)
		return
	}
	state.Phase = "running"
	m.state = state
	m.updateProgress(state)
}

func (m *Manager) finishRun(ctx context.Context, request finalRequest) {
	waitCtx, cancel := context.WithTimeout(request.ctx, m.options.ShutdownTimeout)
	defer cancel()
	state := m.state
	state.Phase = "end"
	state.FinishedAt = request.finishedAt
	state.Error = request.err
	state.Elapsed = 0
	if !state.StartedAt.IsZero() {
		state.Elapsed = max(0, state.FinishedAt.Sub(state.StartedAt))
	}
	latest, err := snapshotState(ctx, state)
	if err != nil {
		m.err = err
		fs.Logf(nil, "notification: failed to collect final statistics: %v", err)
	} else {
		state = latest
	}
	m.state = state
	m.sendFinal(state)
	if err := m.wait(waitCtx); err != nil {
		m.err = errors.Join(m.err, err)
		return
	}
	for i, w := range m.workers {
		if w.err != nil {
			m.err = errors.Join(m.err, fmt.Errorf("notification provider %d: %w", i+1, w.err))
		}
	}
}

func (w *worker) notify(ctx context.Context, state State) error {
	requestCtx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	err := w.notifier.Notify(requestCtx, state)
	if err != nil {
		fs.Logf(nil, "notification failed: %v", err)
	}
	return err
}

func (w *worker) finishDelivery(ctx context.Context, state State) {
	// A lost response can cause duplicate messages when a provider retries creation.
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			w.err = err
			return
		}
		w.err = w.notify(ctx, state)
		if w.err == nil || attempt == 2 {
			return
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			w.err = ctx.Err()
			return
		case <-timer.C:
		}
	}
}

func (w *worker) run(ctx context.Context, initial State) {
	defer close(w.done)

	_ = w.notify(ctx, initial)

	for {
		// Prefer an already queued final snapshot over obsolete progress.
		select {
		case state, ok := <-w.final:
			if ok {
				w.finishDelivery(ctx, state)
			}
			return
		default:
		}
		select {
		case <-ctx.Done():
			w.err = ctx.Err()
			return
		case state, ok := <-w.updates:
			if !ok {
				return
			}

			_ = w.notify(ctx, state)
		case state, ok := <-w.final:
			if !ok {
				return
			}
			w.finishDelivery(ctx, state)
			return
		}
	}
}

func (w *worker) trySend(state State) bool {
	select {
	case w.updates <- state:
		return true
	default:
		return false
	}
}

func (w *worker) updateProgress(state State) bool {
	if w.trySend(state) {
		return true
	}

	select {
	case <-w.updates:
	default:
	}

	return w.trySend(state)
}

func (w *worker) trySendFinal(state State) bool {
	select {
	case w.final <- state:
		return true
	default:
		return false
	}
}

func (w *worker) wait(ctx context.Context) error {
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) updateProgress(state State) {
	for _, w := range m.workers {
		ok := w.updateProgress(state)
		if !ok {
			fs.Logf(nil, "manager not updated progress %v", state)
		}
	}
}

func (m *Manager) sendFinal(state State) {
	for _, w := range m.workers {
		ok := w.trySendFinal(state)
		if !ok {
			fs.Logf(nil, "manager not send final state %v", state)
		}
	}
}

func (m *Manager) wait(ctx context.Context) error {
	if m.cancel != nil {
		defer m.cancel()
	}

	for _, w := range m.workers {
		err := w.wait(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}
