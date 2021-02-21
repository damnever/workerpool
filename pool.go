// Package workerpool provides a very flexible implementation of worker(goroutine) pool.
//
// It is extremely useful when we facing "morestack" issue.
// Also some options can enable us to do lockless operations under some circumstances
// by using the worker id.
package workerpool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrNoWorkersAvaiable is returned if there is no workers available
	// in condition of both WaitIfNoWorkersAvailable and CreateIfNoWorkersAvailable are disabled.
	ErrNoWorkersAvaiable = fmt.Errorf("workerpool: not workers available")
	// ErrInvalidWorkerPool indicates WaitDone function has been called.
	ErrInvalidWorkerPool = fmt.Errorf("workerpool: invalid worker pool")
)

// Options configurates the WorkerPool.
type Options struct {
	// Capacity specifies the maximum number of running workers(goroutines), 0 means no limit.
	Capacity uint32
	// IdleTimeout is the maximum amount of time an idle worker(goroutine) will
	// remain idle before terminating itself. Zero means no limit, the workers
	// never die if the pool is valid.
	IdleTimeout time.Duration
	// WaitIfNoWorkersAvailable will wait until there is a worker available if all workers are busy.
	// This option may will conflict with CreateIfNoWorkersAvailable.
	WaitIfNoWorkersAvailable bool
	// CreateIfNoWorkersAvailable will create a ephemeral worker only if all workers are busy
	// and the option WaitIfNoWorkerAvailable is disabled.
	CreateIfNoWorkersAvailable bool
	// CreateWorkerID will inject a worker id into the context of Func.
	// The worker id is useful, for example, we can use it to do some lockless operations
	// when we have fixed number of workers and those workers live long enough.
	CreateWorkerID bool
}

type contextKeyWorkerID struct{}

func injectWorkerID(ctx context.Context, id uint32) context.Context {
	if id != 0 {
		ctx = context.WithValue(ctx, contextKeyWorkerID{}, id)
	}
	return ctx
}

// WorkerID returns the worker id associated with this context.
// Only available if the option CreateWorkerID enabled.
// NOTE that the worker id always starts with 1.
func WorkerID(ctx context.Context) (uint32, bool) {
	if value := ctx.Value(contextKeyWorkerID{}); value != nil {
		return value.(uint32), true
	}
	return 0, false
}

// Func is the type of the function called by worker in the pool.
// It is the caller's responsibility to recover the panic.
type Func func(context.Context)

// WorkerPool offers a pool of reusable workers(goroutines).
//
// NOTE that the WorkerPool does not handle panics.
type WorkerPool struct {
	capacity                   uint32
	idleTimeout                time.Duration
	waitIfNoWorkersAvailable   bool
	createIfNoWorkersAvailable bool
	idpool                     *idpool

	nworkers    uint32
	nephemerals uint32
	nidles      uint32

	taskc   chan task
	stopc   chan struct{}
	stopped int32
	wg      sync.WaitGroup
	factory sync.Pool
	fncpool sync.Pool
}

// New creates a new WorkerPool.
// The pool with default Options has infinite workers and the workers never die.
func New(opts Options) *WorkerPool {
	var idpool *idpool
	if opts.CreateWorkerID {
		idpool = newIDPool()
	}

	return &WorkerPool{
		capacity:                   opts.Capacity,
		idleTimeout:                opts.IdleTimeout,
		waitIfNoWorkersAvailable:   opts.WaitIfNoWorkersAvailable,
		createIfNoWorkersAvailable: opts.CreateIfNoWorkersAvailable,
		idpool:                     idpool,

		nworkers:    0,
		nephemerals: 0,
		nidles:      0,

		taskc:   make(chan task),
		stopc:   make(chan struct{}),
		stopped: 0,
		wg:      sync.WaitGroup{},
		factory: sync.Pool{
			New: func() interface{} {
				return &worker{}
			},
		},
		fncpool: sync.Pool{
			New: func() interface{} {
				return make(chan Func)
			},
		},
	}
}

// Stats contains a list of worker counters.
type Stats struct {
	ResidentWorkers  uint32
	EphemeralWorkers uint32
	IdleWorkers      uint32
}

// Stats returns the current stats.
func (p *WorkerPool) Stats() Stats {
	return Stats{
		ResidentWorkers:  atomic.LoadUint32(&p.nworkers),
		EphemeralWorkers: atomic.LoadUint32(&p.nephemerals),
		IdleWorkers:      atomic.LoadUint32(&p.nidles),
	}
}

// WaitDone waits until all tasks done or the context done.
// The pool becomes unusable(read only) after this operation.
func (p *WorkerPool) WaitDone(ctx context.Context) error {
	if !atomic.CompareAndSwapInt32(&p.stopped, 0, 1) {
		return nil
	}

	close(p.stopc)
	donec := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(donec)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-donec:
		return nil
	}
}

// Submit submits a task and wait until it acquired by an available worker
// or wait until the context done if WaitIfNoWorkersAvailable enabled.
// The "same" ctx will be passed into Func.
func (p *WorkerPool) Submit(ctx context.Context, fn Func) error {
	return p.submit(ctx, task{ctx: ctx, fn: fn})
}

// SubmitConcurrentDependent submits multiple concurrent dependent tasks and wait until
// all of them are acquired by available workers or wait until the
// context done if WaitIfNoWorkersAvailable enabled.
// The "same" ctx will be passed into Func.
func (p *WorkerPool) SubmitConcurrentDependent(ctx context.Context, fns ...Func) error {
	n := len(fns)
	if n == 0 {
		return nil
	}
	if n == 1 { // We ignored CreateIfNoWorkerAvailable, because the ctx may fail.
		return p.Submit(ctx, fns[0])
	}

	var (
		futures = make([]futureTask, 0, n)
		settled bool
		err     error
	)
	defer func() {
		if err != nil {
			for _, future := range futures {
				future.cancel()
				if !settled { // Try to recycle the channels.
					p.fncpool.Put(future.funcc)
				}
			}
		}
	}()
	for i := 0; i < n; i++ {
		future := newFutureTaskFrom(p.fncpool.Get().(chan Func))
		futures = append(futures, future)
		if err = p.submit(ctx, task{ctx: ctx, future: future}); err != nil {
			return err
		}
	}

	// We can not reuse futureTask.taskc here since there is a rare chance that
	// the stale futureTask may get notified.
	settled = true
	for i, fn := range fns {
		// Dead lock is impossible here since all tasks has already took a placeholder,
		// but this cloud make stop process longer.
		// If we check the stopc/ctx.Done(), the case cloud become complicated,
		// because we can not cancel other tasks unless all futureTask are still waiting.
		futures[i].send(fn)
	}
	return nil
}

// Extra per task options??
func (p *WorkerPool) submit(ctx context.Context, task task) error { //nolint:gocyclo
	if atomic.LoadInt32(&p.stopped) == 1 {
		return ErrInvalidWorkerPool
	}

	select {
	case <-p.stopc:
		return ErrInvalidWorkerPool
	case <-ctx.Done():
		return ctx.Err()
	case p.taskc <- task:
		return nil
	default:
	}

	for {
		nworkers := atomic.LoadUint32(&p.nworkers)
		if p.capacity == 0 || nworkers < p.capacity {
			if !atomic.CompareAndSwapUint32(&p.nworkers, nworkers, nworkers+1) {
				continue // Conflicted, try again.
			}
			// Make this job run first since we do not know
			// when the goroutine will be scheduled and start running.
			p.asyncWork(false, task)
			return nil
		}

		if p.waitIfNoWorkersAvailable {
			select {
			case <-p.stopc:
				return ErrInvalidWorkerPool
			case <-ctx.Done():
				return ctx.Err()
			case p.taskc <- task:
				return nil
			}
		} else {
			select {
			case <-p.stopc:
				return ErrInvalidWorkerPool
			case <-ctx.Done():
				return ctx.Err()
			case p.taskc <- task:
				return nil
			default:
				if p.createIfNoWorkersAvailable {
					p.asyncWork(true, task)
					return nil
				}
				return ErrNoWorkersAvaiable
			}
		}
	}
}

func (p *WorkerPool) asyncWork(ephemeral bool, initTask task) {
	worker := p.makeWorker()

	p.wg.Add(1)
	go func() {
		atomic.AddUint32(&p.nidles, 1)
		if ephemeral {
			atomic.AddUint32(&p.nephemerals, 1)
		}
		defer func() {
			atomic.AddUint32(&p.nidles, ^uint32(0))
			if ephemeral {
				atomic.AddUint32(&p.nephemerals, ^uint32(0))
			} else {
				atomic.AddUint32(&p.nworkers, ^uint32(0)) // Subtract 1.
			}

			p.recycleWorker(worker)
			p.wg.Done()
		}()

		worker.run(&p.nidles, ephemeral, initTask)
	}()
}

func (p *WorkerPool) makeWorker() *worker {
	w := p.factory.Get().(*worker)
	if p.idpool != nil {
		w.id = p.idpool.get()
	}
	w.idleTimeout = p.idleTimeout
	w.stopc = p.stopc
	w.taskc = p.taskc
	return w
}

func (p *WorkerPool) recycleWorker(w *worker) {
	if p.idpool != nil {
		p.idpool.put(w.id)
	}
	p.factory.Put(w)
}

type worker struct {
	id          uint32
	idleTimeout time.Duration
	stopc       <-chan struct{}
	taskc       <-chan task
	timer       *time.Timer
}

func (w *worker) run(idleCounter *uint32, ephemeral bool, initTask task) {
	timer := w.timer
	defer func() {
		if timer != nil {
			// Try our best to make sure the timer is stopped and clean..
			timer.Stop()
			select {
			case <-timer.C:
			default:
			}
			// Set it.
			w.timer = timer
		}
	}()

	var (
		timerc   <-chan time.Time
		nexttask = initTask
	)
	for {
		atomic.AddUint32(idleCounter, ^uint32(0))
		nexttask.execute(func(ctx context.Context) context.Context {
			// Inject values into context.
			if w.id != 0 {
				ctx = injectWorkerID(ctx, w.id)
			}
			return ctx
		})
		atomic.AddUint32(idleCounter, 1)

		if ephemeral {
			return
		}

		if w.idleTimeout > 0 {
			if timer == nil {
				timer = time.NewTimer(w.idleTimeout)
			} else {
				// Careful, this piece of code may cause problems!!!
				// Ref: https://github.com/golang/go/issues/11513
				// We have not drained the t.C, so this is ok.
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(w.idleTimeout)
			}
			timerc = timer.C
		}

		select {
		case <-w.stopc:
			return
		case <-timerc:
			return
		case nexttask = <-w.taskc:
		}
	}
}

type task struct {
	ctx    context.Context
	fn     Func
	future futureTask
}

func (t task) execute(inject func(context.Context) context.Context) {
	if t.fn != nil {
		t.fn(inject(t.ctx))
	} else if fn, ok := t.future.resolve(); ok {
		fn(inject(t.ctx))
	}
}

type futureTask struct {
	cancelc chan struct{}
	funcc   chan Func
}

func newFutureTaskFrom(fnc chan Func) futureTask {
	return futureTask{
		cancelc: make(chan struct{}),
		funcc:   fnc,
	}
}

func (f futureTask) resolve() (Func, bool) {
	select {
	// case <-stopc: // XXX: check out stopc could make it triky so we skip it.
	case <-f.cancelc:
		return nil, false
	case fn := <-f.funcc:
		return fn, true
	}
}

func (f futureTask) send(taskFunc Func) {
	f.funcc <- taskFunc
}

func (f futureTask) cancel() {
	close(f.cancelc)
}

type idpool struct {
	lock     sync.Mutex
	recycled map[uint32]bool
	next     uint32
}

func newIDPool() *idpool {
	return &idpool{
		lock:     sync.Mutex{},
		recycled: make(map[uint32]bool),
		next:     1,
	}
}

func (p *idpool) get() uint32 {
	p.lock.Lock()
	defer p.lock.Unlock()

	for id := range p.recycled {
		delete(p.recycled, id)
		return id
	}

	next := p.next
	p.next++
	return next
}

func (p *idpool) put(id uint32) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if id < 1 || id >= p.next || p.recycled[id] {
		panic("invalid id")
	}
	if id == p.next-1 {
		p.next--
	} else {
		p.recycled[id] = true
	}
}
