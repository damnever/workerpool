// Package workerpool provides a handy and fast worker(goroutine) pool.
//
// It is extremely useful when we facing "morestack" issue.
// Also some options can enable us to do lockless operations under some circumstances
// by using the worker id.
package workerpool

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
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
	// Capacity specifies the maximum number of resident running workers(goroutines),
	// 0 means no limit.
	Capacity uint32
	// IdleTimeout is the maximum amount of time a worker(goroutine) will
	// remain idle before terminating itself. Zero means no limit, the workers
	// never die if the pool is valid.
	IdleTimeout time.Duration
	// ResetInterval defines how often the worker(goroutine) must be restarted,
	// zero to disable it.
	// With this options enabled, a worker can reset its stack so that large stacks
	// don't live in memory forever, 25% jitter will be applied.
	ResetInterval time.Duration
	// WaitIfNoWorkersAvailable will wait until there is a worker available
	// if all resident workers are busy.
	// It only works if the option Capacity greater than zero.
	// This option will conflict with CreateIfNoWorkersAvailable.
	WaitIfNoWorkersAvailable bool
	// CreateIfNoWorkersAvailable will create an ephemeral worker only
	// if all resident workers are busy.
	// It only works if the option Capacity greater than zero and the option
	// WaitIfNoWorkerAvailable is disabled.
	CreateIfNoWorkersAvailable bool
	// CreateWorkerID will inject a worker id into the context of Func.
	// It may be useful, for example, we can use it to do some lockless operations
	// under some circumstances when we have fixed number of workers and those workers live long enough.
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

// Func is the type of function called by worker in the pool.
// It is the caller's responsibility to recover the panic.
type Func func(context.Context)

// WorkerPool offers a pool of reusable workers(goroutines).
//
// NOTE that the WorkerPool does not handle panics.
type WorkerPool struct {
	capacity                   int
	idleTimeout                time.Duration
	resetInterval              time.Duration
	waitIfNoWorkersAvailable   bool
	createIfNoWorkersAvailable bool
	idpool                     *idpool

	nephemerals uint32
	nwaiters    uint32

	lock        sync.RWMutex
	workers     workerList
	idleWorkers workerQueue
	waiters     waiterList
	invalid     bool

	rand       *rand.Rand
	randlock   sync.Mutex
	stopc      chan struct{}
	wg         sync.WaitGroup
	workerPool sync.Pool
	waiterPool sync.Pool
	funccPool  sync.Pool
}

// New creates a new WorkerPool.
// The pool with default(empty) Options has infinite workers and the workers never die.
func New(opts Options) *WorkerPool {
	p := &WorkerPool{
		capacity:                   int(opts.Capacity),
		idleTimeout:                opts.IdleTimeout,
		resetInterval:              opts.ResetInterval,
		waitIfNoWorkersAvailable:   opts.WaitIfNoWorkersAvailable,
		createIfNoWorkersAvailable: opts.CreateIfNoWorkersAvailable,
		idpool:                     newIDPool(opts.CreateWorkerID),

		nephemerals: 0,
		lock:        sync.RWMutex{},
		workers:     workerList{},
		idleWorkers: workerQueue{},
		waiters:     waiterList{},
		invalid:     false,

		rand:       rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec
		randlock:   sync.Mutex{},
		stopc:      make(chan struct{}),
		wg:         sync.WaitGroup{},
		workerPool: sync.Pool{},
		waiterPool: sync.Pool{},
		funccPool:  sync.Pool{},
	}
	if p.idleTimeout > 0 {
		go p.workerGCLoop()
	}
	return p
}

// Stats contains a list of worker counters.
type Stats struct {
	// ResidentWorkers counts the number of resident workers.
	ResidentWorkers uint32
	// EphemeralWorkers counts the number of ephemeral workers when
	// the option CreateIfNoWorkersAvailable is enabled.
	EphemeralWorkers uint32
	// IdleWorkers counts all idle workers including any newly created workers.
	IdleWorkers uint32
	// PendingSubmits counts all pending Submit(*).
	PendingSubmits uint32
}

// Stats returns the current stats.
func (p *WorkerPool) Stats() Stats {
	p.lock.RLock()
	nworkers := p.workers.length()
	nidles := p.idleWorkers.length()
	p.lock.RUnlock()
	return Stats{
		ResidentWorkers:  uint32(nworkers),
		EphemeralWorkers: atomic.LoadUint32(&p.nephemerals),
		IdleWorkers:      uint32(nidles),
		PendingSubmits:   atomic.LoadUint32(&p.nwaiters),
	}
}

// WaitDone waits until all tasks done or the context done.
// The pool becomes unusable(read only) after this operation.
// If you want to wait multiple times, using an extra sync.WaitGroup.
// NOTE it panics if ctx==nil, pass context.TODO() or context.Background() instead.
func (p *WorkerPool) WaitDone(ctx context.Context) error {
	p.lock.Lock()
	alreadyInvalid := p.invalid
	if !alreadyInvalid {
		p.invalid = true
	}
	p.lock.Unlock()
	if alreadyInvalid {
		return nil
	}

	close(p.stopc)
	donec := make(chan struct{})
	go func() {
		p.stopAllWorkers()
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

// Submit submits a task and waits until it acquired by an available worker
// or wait until the context done if WaitIfNoWorkersAvailable enabled.
// The "same" ctx will be passed into Func.
// NOTE it panics if ctx==nil, pass context.TODO() or context.Background() instead.
func (p *WorkerPool) Submit(ctx context.Context, fn Func) error {
	return p.submit(ctx, task{ctx: ctx, fn: fn})
}

// SubmitConcurrentDependent submits multiple *concurrent dependent* tasks and waits until
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
					p.funccPool.Put(future.funcc)
				}
			}
		}
	}()
	for i := 0; i < n; i++ {
		fnc, _ := p.funccPool.Get().(chan Func)
		future := newFutureTaskFrom(fnc)
		futures = append(futures, future)
		if err = p.submit(ctx, task{ctx: ctx, future: future}); err != nil {
			return err
		}
	}

	// We can not reuse futureTask.funcc here since there is a rare chance that
	// the stale futureTask may get notified by the reused channel with newest event.
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
func (p *WorkerPool) submit(ctx context.Context, task task) error { //nolint:gocyclo,gocognit
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	atomic.AddUint32(&p.nwaiters, 1)
	defer atomic.AddUint32(&p.nwaiters, ^uint32(0))

	wk, invalid := p.getIdleWorkerNoWait()
	if invalid {
		return ErrInvalidWorkerPool
	}
	if wk != nil {
		select {
		case <-p.stopc:
			return ErrInvalidWorkerPool
		case <-ctx.Done():
			// N.B. putIdleWorker will update worker's idle state (delay the idleTimeout),
			// but this is ok because if pool is busy, recreating a worker is wastful,
			// if pool is idle, delay is acceptable.
			p.putIdleWorker(wk)
			return ctx.Err()
		case wk.taskc <- task:
			return nil
		}
	}

	for {
		// Make this job run first since we do not know
		// when the goroutine will be scheduled and start running.
		created, invalid := p.spawnWorker(false, task)
		if invalid {
			return ErrInvalidWorkerPool
		}
		if created {
			return nil
		}

		if p.waitIfNoWorkersAvailable {
			var w *waiter
			wk, w, invalid = p.getIdleWorkerOrWaiter()
			if invalid {
				return ErrInvalidWorkerPool
			}
			if wk == nil && w != nil {
				select {
				case <-p.stopc:
					p.removeWaiter(w)
					return ErrInvalidWorkerPool
				case <-ctx.Done():
					p.removeWaiter(w)
					return ctx.Err()
				case wk = <-w.C:
					p.removeWaiter(w)
					// We may use sync.Cond here, but the Wait is not cancelable.
				}
			}
			if wk == nil {
				// Nil value indicates that the pool is not full,
				// we should try to create one.
				continue
			}
			select {
			case <-p.stopc:
				return ErrInvalidWorkerPool
			case <-ctx.Done():
				p.putIdleWorker(wk)
				return ctx.Err()
			case wk.taskc <- task:
				return nil
			}
		}

		if p.createIfNoWorkersAvailable {
			if created, invalid := p.spawnWorker(true, task); !created || invalid {
				panic("fail to spawn ephemeral worker")
			}
			return nil
		}
		return ErrNoWorkersAvaiable
	}
}

func (p *WorkerPool) getIdleWorkerNoWait() (*worker, bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.invalid {
		return nil, true
	}
	var wk *worker
	if p.idleWorkers.length() > 0 {
		wk = p.idleWorkers.get()
	}
	return wk, false
}

func (p *WorkerPool) getIdleWorkerOrWaiter() (*worker, *waiter, bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if p.invalid {
		return nil, nil, true
	}
	if p.idleWorkers.length() > 0 {
		wk := p.idleWorkers.get()
		return wk, nil, false
	}
	if p.workers.length() < p.capacity {
		return nil, nil, false
	}

	w, ok := p.waiterPool.Get().(*waiter)
	if !ok {
		w = newWaiter()
	}
	p.waiters.pushback(w)
	return nil, w, false
}

func (p *WorkerPool) putIdleWorker(wk *worker) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if p.invalid {
		return
	}

	wk.resetIdleState(p.idleTimeout)
	p.idleWorkers.put(wk)

	// Try notify waiters.
	for p.idleWorkers.length() > 0 && p.waiters.length() > 0 {
		w := p.waiters.popfront()
		wk := p.idleWorkers.get()
		w.C <- wk
	}
}

func (p *WorkerPool) removeWaiter(w *waiter) {
	p.lock.Lock()
	p.waiters.remove(w)
	p.lock.Unlock()

	// We removed it from list so no one get chance to hold it
	select {
	case <-w.C: // Drain the channel so we can safely reuse it.
	default:
	}
	p.waiterPool.Put(w)
}

func (p *WorkerPool) stopAllWorkers() {
	p.lock.Lock()
	if !p.invalid {
		panic("WorkerPool still valid")
	}
	workers := p.workers.clear()
	p.idleWorkers.clear()
	p.waiters.clear()
	p.lock.Unlock()

	nworkers := len(workers)
	if nworkers == 0 {
		return
	}
	wg := sync.WaitGroup{}
	maxprocs := runtime.GOMAXPROCS(-1)
	split := (nworkers + maxprocs - 1) / maxprocs
	for i := 0; i < maxprocs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k, n := i*split, (i+1)*split; k < n && k < nworkers; k++ {
				workers[k].stop()
			}
		}(i)
	}
	wg.Wait()
}

func (p *WorkerPool) workerGCLoop() {
	// N.B. we can let worker themselves to hanle timeouts,
	// but those logic is too expensive for workers,
	// huge number of workers with long-live select cases will consume
	// a lot of CPU, so we should make worker as light as possible.
	interval := p.idleTimeout
	if interval >= 10*time.Second {
		interval /= 2
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	const bufcap = 2048
	workers := make([]*worker, 0, bufcap)
	for {
		select {
		case <-p.stopc:
			return
		case <-ticker.C:
		}

		now := time.Now()
		p.lock.Lock()
		index := p.idleWorkers.binsearch(func(w *worker) bool { return !w.expired(now) })
		p.idleWorkers.movebefore(index, &workers)
		for _, w := range workers {
			p.workers.remove(w) // Remove to avoid dead lock since taskc has no buffer.
		}
		p.lock.Unlock()

		for _, w := range workers {
			w.stop()
		}
		for i := range workers {
			workers[i] = nil // Avoid memory leak.
		}
		workers = workers[:bufcap] // Keep a small bufffer only.
	}
}

func (p *WorkerPool) spawnWorker(ephemeral bool, firstTask task) (bool, bool) {
	newWorker := func() *worker {
		wk, ok := p.workerPool.Get().(*worker)
		if !ok {
			wk = &worker{}
		}
		return wk
	}

	var wk *worker
	if ephemeral {
		wk = newWorker()
	} else {
		p.lock.Lock()
		allow := p.capacity <= 0 || p.workers.length() < p.capacity
		invalid := p.invalid
		if invalid || !allow {
			p.lock.Unlock()
			return allow, invalid
		}
		wk = newWorker()
		p.workers.pushback(wk)
		p.lock.Unlock()
	}
	id := p.idpool.get()
	wk.init(id, ephemeral)

	if ephemeral {
		atomic.AddUint32(&p.nephemerals, 1)
	}
	p.wg.Add(1)
	cleanupFunc := func() {
		p.wg.Done()
		if ephemeral {
			atomic.AddUint32(&p.nephemerals, ^uint32(0))
		} else {
			p.lock.Lock()
			p.workers.remove(wk)
			for remain := p.capacity - p.workers.length(); remain > 0 && p.waiters.length() > 0; remain-- {
				w := p.waiters.popfront()
				// Nil to signal waiters to try to create a worker.
				w.C <- nil
			}
			p.lock.Unlock()
		}

		p.idpool.put(id)
		p.workerPool.Put(wk)
	}
	go wk.run(firstTask, cleanupFunc, p.putIdleWorker, p.jitteredResetInterval())
	return true, false
}

func (p *WorkerPool) jitteredResetInterval() time.Duration {
	if p.resetInterval <= 0 {
		return 0
	}

	p.randlock.Lock()
	factor := p.rand.Float64()
	p.randlock.Unlock()

	f := float64(p.resetInterval)
	delta := 0.25 * f
	min := f - delta
	max := f + delta
	return time.Duration(min + (max-min)*factor)
}

var _emptytask = task{empty: true}

type task struct {
	empty bool

	ctx    context.Context
	fn     Func
	future futureTask
}

func (t task) isempty() bool {
	return t.empty
}

func (t task) execute(inject func(context.Context) context.Context) {
	if t.isempty() {
		return
	}

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
	if fnc == nil {
		fnc = make(chan Func)
	}
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

type worker struct {
	id        uint32
	ephemeral bool
	taskc     chan task
	expiredAt time.Time

	prev *worker
	next *worker
	list *workerList
}

func (w *worker) init(id uint32, ephemeral bool) {
	w.id = id
	w.ephemeral = ephemeral
	w.expiredAt = time.Time{}
	if w.taskc == nil && !ephemeral {
		w.taskc = make(chan task)
	}
}

func (w *worker) resetIdleState(idleTimeout time.Duration) {
	if idleTimeout > 0 {
		w.expiredAt = time.Now().Add(idleTimeout)
	}
}

func (w *worker) expired(now time.Time) bool {
	return !w.expiredAt.IsZero() && w.expiredAt.Before(now)
}

func (w *worker) stop() {
	w.taskc <- _emptytask
}

func (w *worker) run(nexttask task, cleanup func(), markAsIdle func(*worker), resetInterval time.Duration) {
	// NOTE: can not use defer to do cleanup work since we may restart the goroutine.
	startAt := time.Now()
	for {
		nexttask.execute(func(ctx context.Context) context.Context {
			// Inject values into context.
			if w.id != 0 {
				ctx = injectWorkerID(ctx, w.id)
			}
			return ctx
		})
		nexttask = _emptytask // Avoid memory leak.

		if w.ephemeral {
			cleanup()
			return // Terminating.
		}

		if resetInterval > 0 { // Guard for less syscalls.
			if time.Since(startAt) >= resetInterval {
				// Restart the goroutine to reset the stack size.
				go w.run(_emptytask, cleanup, markAsIdle, resetInterval)
				return
			}
		}

		markAsIdle(w)
		nexttask = <-w.taskc
		if nexttask.isempty() {
			cleanup()
			return // Terminating.
		}
	}
}

// This queue implementation is copied and modified from:
// https://github.com/eapache/queue/blob/master/queue.go
type workerQueue struct {
	workers []*worker
	head    int
	tail    int
	count   int
	cap     int
}

func (q *workerQueue) indexof(n int) int {
	return n % q.cap
}

func (q *workerQueue) clear() {
	*q = workerQueue{}
}

func (q *workerQueue) binsearch(f func(*worker) bool) int {
	// Copied and modified from sort.Search.
	i, j := 0, q.count
	for i < j {
		h := int(uint(i+j) >> 1) // avoid overflow when computing h.
		// i ≤ h < j.
		if !f(q.workers[q.indexof(h+q.head)]) {
			i = h + 1 // preserves f(i-1) == false.
		} else {
			j = h // preserves f(j) == true.
		}
	}
	// i == j, f(i-1) == false, and f(j) (= f(i)) == true  =>  answer is i.
	return i
}

func (q *workerQueue) movebefore(idx int, buffer *[]*worker) {
	*buffer = (*buffer)[:0]
	if q.count == 0 || idx == 0 {
		return
	}

	if q.head < q.tail {
		max := q.head + idx
		if max > q.tail {
			max = q.tail
		}
		*buffer = append(*buffer, q.workers[q.head:max]...)
	} else { // NOTE: if workers is full, tail == head.
		max := q.head + idx
		if max > q.cap {
			max = q.cap
		}
		*buffer = append(*buffer, q.workers[q.head:max]...)
		remain := idx - len(*buffer)
		if remain > q.tail {
			remain = q.tail
		}
		*buffer = append(*buffer, q.workers[0:remain]...)
	}

	n := len(*buffer)
	for i := q.head; i < q.head+n; i++ {
		q.workers[q.indexof(i)] = nil
	}
	q.head = q.indexof(q.head + n)
	q.count -= n
	q.tail = q.indexof(q.head + q.count)
	q.tryresize()
}

func (q *workerQueue) put(w *worker) {
	q.tryresize()

	q.workers[q.tail] = w
	q.tail = q.indexof(q.tail + 1)
	q.count++
}

func (q *workerQueue) get() *worker {
	if q.count == 0 {
		panic("workerQueue: empty queue")
	}
	w := q.workers[q.head]
	q.workers[q.head] = nil
	q.head = q.indexof(q.head + 1)
	q.count--

	q.tryresize()
	return w
}

func (q *workerQueue) length() int {
	return q.count
}

func (q *workerQueue) tryresize() {
	const highWatermark = 1024
	const lowWatermark = 16
	needresize := false
	if q.count == q.cap {
		if q.cap == 0 {
			q.cap = 1
		}
		if bufcap := (q.cap << 1); bufcap <= highWatermark {
			q.cap = bufcap
		} else {
			q.cap += highWatermark
		}
		needresize = true
	} else if q.cap > lowWatermark && (q.count<<2) == q.cap {
		q.cap >>= 1
		needresize = true
	}
	if !needresize {
		return
	}

	workers := make([]*worker, q.cap, q.cap)
	if q.head < q.tail {
		copy(workers, q.workers[q.head:q.tail])
	} else { // NOTE: if workers is full, tail == head.
		n := copy(workers, q.workers[q.head:])
		copy(workers[n:], q.workers[:q.tail])
	}
	q.workers = workers
	q.head = 0
	q.tail = q.count
}

type workerList struct {
	// A dummy node has the next points to the head of the list,
	// the preve points to the tail of the list.
	dummy worker
	count int
}

func (l *workerList) lazyinit() {
	if l.dummy.next == nil {
		l.dummy.next = &l.dummy
		l.dummy.prev = &l.dummy
		l.count = 0
	}
}

func (l *workerList) length() int {
	return l.count
}

func (l *workerList) clear() []*worker {
	if l.count == 0 {
		return nil
	}

	workers := make([]*worker, 0, l.count)
	for e := l.dummy.next; e.list != nil && e != &l.dummy; {
		workers = append(workers, e)
		pe := e
		e = e.next
		l.remove(pe)
	}
	return workers
}

func (l *workerList) popfront() *worker {
	if l.count == 0 {
		panic("workerList: empty")
	}
	e := l.dummy.next
	l.remove(e)
	return e
}

func (l *workerList) pushback(e *worker) {
	l.lazyinit()
	if e.list != nil {
		panic("workerList: insert duplicate element")
	}

	tail := l.dummy.prev
	e.prev = tail
	e.next = tail.next
	e.prev.next = e
	e.next.prev = e
	e.list = l
	l.count++
}

func (l *workerList) remove(e *worker) bool { //nolint:unparam
	if l != e.list {
		return false
	}
	e.prev.next = e.next
	e.next.prev = e.prev
	e.prev = nil
	e.next = nil
	e.list = nil
	l.count--
	if l.count < 0 {
		panic("workerList: negative count")
	}
	return true
}

type waiter struct {
	C chan *worker

	list *waiterList
	prev *waiter
	next *waiter
}

func newWaiter() *waiter {
	return &waiter{
		C: make(chan *worker, 1), // Buffer needed.
	}
}

type waiterList struct {
	// A dummy node has the next points to the head of the list,
	// the preve points to the tail of the list.
	dummy waiter
	count int
}

func (l *waiterList) lazyinit() {
	if l.dummy.next == nil {
		l.dummy.next = &l.dummy
		l.dummy.prev = &l.dummy
		l.count = 0
	}
}

func (l *waiterList) clear() {
	if l.count == 0 {
		return
	}
	for e := l.dummy.next; e.list != nil && e != &l.dummy; {
		pe := e
		e = e.next
		l.remove(pe)
	}
}

func (l *waiterList) length() int {
	return l.count
}

func (l *waiterList) popfront() *waiter {
	if l.count == 0 {
		panic("waiterList: empty")
	}
	e := l.dummy.next
	l.remove(e)
	return e
}

func (l *waiterList) pushback(e *waiter) {
	l.lazyinit()
	if e.list != nil {
		panic("waiterList: insert duplicate element")
	}

	tail := l.dummy.prev
	e.prev = tail
	e.next = tail.next
	e.prev.next = e
	e.next.prev = e
	e.list = l
	l.count++
}

func (l *waiterList) remove(e *waiter) bool { //nolint:unparam
	if l != e.list {
		return false
	}
	e.prev.next = e.next
	e.next.prev = e.prev
	e.prev = nil
	e.next = nil
	e.list = nil
	l.count--
	if l.count < 0 {
		panic("waiterList: negative count")
	}
	return true
}

type idpool struct {
	lock     sync.Mutex
	recycled map[uint32]bool
	next     uint32
	enabled  bool
}

func newIDPool(enable bool) *idpool {
	return &idpool{
		recycled: make(map[uint32]bool),
		next:     1,
		enabled:  enable,
	}
}

func (p *idpool) get() uint32 {
	if !p.enabled {
		return 0
	}

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
	if !p.enabled {
		return
	}

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
