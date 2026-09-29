package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// ErrClosed is returned by Writer.Do after Close has been called.
var ErrClosed = errors.New("store: writer closed")

const (
	// queueSize bounds the number of jobs waiting for the writer. Callers
	// block (subject to their context) when it is full.
	queueSize = 256
	// maxBatch is the most jobs committed in one transaction.
	maxBatch = 128
)

// TxFunc performs writes inside tx. It must not call tx.Commit,
// tx.Rollback or DB.Write (the writer would deadlock). ctx is the context
// passed to Do.
type TxFunc func(ctx context.Context, tx *sql.Tx) error

type job struct {
	ctx  context.Context
	fn   TxFunc
	errc chan error
}

// Writer serializes all database writes on one goroutine. Jobs that are
// queued together are committed in a single transaction; each job runs inside
// its own savepoint, so a failing job is rolled back without affecting the
// others in the batch.
type Writer struct {
	db   *sql.DB
	jobs chan *job
	done chan struct{}

	mu     sync.RWMutex // guards closed and sends on jobs
	closed bool

	// batchHook, if set, is called with the size of each batch before it
	// runs. Tests only.
	batchHook func(size int)
}

func newWriter(db *sql.DB) *Writer {
	w := &Writer{
		db:   db,
		jobs: make(chan *job, queueSize),
		done: make(chan struct{}),
	}
	go w.loop()
	return w
}

// Do queues fn and waits until its transaction has committed or failed. It
// returns fn's error, a commit error, ctx.Err() if ctx ends before fn starts,
// or ErrClosed. Once Do has queued the job it always waits for the outcome,
// so a nil return means the write is durable to the configured sync level.
func (w *Writer) Do(ctx context.Context, fn TxFunc) error {
	j := &job{ctx: ctx, fn: fn, errc: make(chan error, 1)}

	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		return ErrClosed
	}
	select {
	case w.jobs <- j:
		w.mu.RUnlock()
	case <-ctx.Done():
		w.mu.RUnlock()
		return ctx.Err()
	}
	return <-j.errc
}

// Close stops accepting jobs, waits for queued jobs to finish, and returns.
// It is safe to call more than once.
func (w *Writer) Close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.jobs)
	}
	w.mu.Unlock()
	<-w.done
}

func (w *Writer) loop() {
	defer close(w.done)
	batch := make([]*job, 0, maxBatch)
	for j := range w.jobs {
		batch = append(batch[:0], j)
	fill:
		for len(batch) < maxBatch {
			select {
			case next, ok := <-w.jobs:
				if !ok {
					break fill
				}
				batch = append(batch, next)
			default:
				break fill
			}
		}
		if w.batchHook != nil {
			w.batchHook(len(batch))
		}
		w.runBatch(batch)
	}
}

func (w *Writer) runBatch(batch []*job) {
	errs := make([]error, len(batch))
	defer func() {
		for i, j := range batch {
			j.errc <- errs[i]
		}
	}()

	// Jobs whose context already ended are not run.
	run := make([]int, 0, len(batch))
	for i, j := range batch {
		if err := j.ctx.Err(); err != nil {
			errs[i] = err
			continue
		}
		run = append(run, i)
	}
	if len(run) == 0 {
		return
	}

	// failAll gives err to every job that has not already failed on its own.
	failAll := func(err error) {
		for _, i := range run {
			if errs[i] == nil {
				errs[i] = err
			}
		}
	}

	tx, err := w.db.BeginTx(context.Background(), nil)
	if err != nil {
		failAll(fmt.Errorf("store: begin write transaction: %w", err))
		return
	}
	for _, i := range run {
		err := runJob(tx, batch[i])
		var txErr errTx
		if errors.As(err, &txErr) {
			_ = tx.Rollback()
			failAll(err)
			return
		}
		errs[i] = err
	}
	if err := tx.Commit(); err != nil {
		failAll(fmt.Errorf("store: commit write transaction: %w", err))
	}
}

// runJob executes one job inside a savepoint. A panic in fn is converted to
// an error so the writer goroutine survives.
func runJob(tx *sql.Tx, j *job) (err error) {
	if _, err := tx.ExecContext(context.Background(), "SAVEPOINT job"); err != nil {
		return errTx{err}
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("store: write job panicked: %v", r)
		}
		if err != nil {
			if _, rbErr := tx.ExecContext(context.Background(), "ROLLBACK TO job"); rbErr != nil {
				err = errTx{withJobErr(rbErr, err)}
				return
			}
		}
		if _, relErr := tx.ExecContext(context.Background(), "RELEASE job"); relErr != nil {
			err = errTx{withJobErr(relErr, err)}
		}
	}()
	return j.fn(j.ctx, tx)
}

// withJobErr keeps the job's own error in the message when the savepoint
// statement that follows it fails too. SQLite drops the savepoints of a
// transaction when a statement fails with an I/O error, a full disk or an
// interrupt, so in that case the savepoint error is only the consequence and
// the job's error is the cause worth reading.
func withJobErr(txErr, jobErr error) error {
	if jobErr == nil {
		return txErr
	}
	return fmt.Errorf("%w; the job had failed with: %w", txErr, jobErr)
}

// errTx marks a failure of the transaction itself (as opposed to a job's own
// error); the whole batch must then be abandoned.
type errTx struct{ err error }

func (e errTx) Error() string { return "store: write transaction: " + e.err.Error() }
func (e errTx) Unwrap() error { return e.err }
