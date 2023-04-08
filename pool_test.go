package workerpool

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func emptyFunc(context.Context) {}

func TestWorkerPool_OptionCapacity(t *testing.T) {
	t.Parallel()

	f := func(context.Context) { time.Sleep(50 * time.Millisecond) }
	{
		pool := New(Options{Capacity: 1})
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		require.Nil(t, pool.Submit(ctx, func(context.Context) { time.Sleep(100 * time.Millisecond) }))
		require.Equal(t, ErrNoWorkersAvailable, pool.Submit(ctx, emptyFunc))
		require.Nil(t, pool.WaitDone(context.TODO()))
	}
	{
		pool := New(Options{Capacity: 2})
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		require.Nil(t, pool.SubmitConcurrentDependent(ctx, f, f))
		require.Equal(t, ErrNoWorkersAvailable, pool.SubmitConcurrentDependent(ctx, emptyFunc))
		require.Nil(t, pool.WaitDone(context.TODO()))
	}
	{
		pool := New(Options{Capacity: 1})
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		require.Nil(t, pool.SubmitConcurrentDependent(ctx, emptyFunc))
		require.Equal(t, ErrNoWorkersAvailable, pool.SubmitConcurrentDependent(ctx, f, f))
		require.Nil(t, pool.WaitDone(context.TODO()))
	}
}

func TestWorkerPool_OptionIdleTimeout(t *testing.T) {
	t.Parallel()

	pool := New(Options{Capacity: 3, IdleTimeout: 200 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	fn := func(context.Context) { time.Sleep(100 * time.Millisecond) }
	require.Nil(t, pool.Submit(ctx, fn))
	require.Nil(t, pool.SubmitConcurrentDependent(ctx, fn, fn))
	time.Sleep(50 * time.Millisecond)
	stats := pool.Stats()
	require.Equal(t, uint32(3), stats.ResidentWorkers)

	stopc := make(chan struct{})
	donec := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopc:
				return
			case <-time.After(time.Millisecond):
			}
			stats = pool.Stats()
			if stats.ResidentWorkers == 0 {
				close(donec)
				return
			}
		}
	}()
	select {
	case <-time.After(600 * time.Millisecond):
		t.Fatalf("timed out to waiting idle workers")
	case <-donec:
	}
	require.Equal(t, uint32(0), stats.ResidentWorkers)
	require.Nil(t, pool.WaitDone(context.TODO()))
}

func TestWorkerPool_OptionWaitIfNoWorkersAvailable(t *testing.T) {
	t.Parallel()

	testF := func(capacity uint32, gof func(pool *WorkerPool), uselessCreate bool) {
		pool := New(Options{
			Capacity:                   capacity,
			IdleTimeout:                300 * time.Millisecond,
			WaitIfNoWorkersAvailable:   true,
			CreateIfNoWorkersAvailable: uselessCreate,
		})

		count := uint32(0)
		donecountc := make(chan struct{})
		defer func() { close(donecountc) }()
		go func() {
			for {
				select {
				case <-donecountc:
					return
				case <-time.After(100 * time.Microsecond):
				}
				stats := pool.Stats()
				nworkers := stats.ResidentWorkers
				for {
					prevcount := atomic.LoadUint32(&count)
					if nworkers <= prevcount {
						break
					}
					if atomic.CompareAndSwapUint32(&count, prevcount, nworkers) {
						break
					}
				}
			}
		}()

		pool.Submit(context.TODO(), func(context.Context) { time.Sleep(80 * time.Millisecond) })
		donec := make(chan struct{})
		go func() {
			gof(pool)
			close(donec)
		}()
		now := time.Now()
		select {
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("timed out")
		case <-donec:
			require.True(t, time.Since(now) >= 60*time.Millisecond)
		}
		require.Nil(t, pool.WaitDone(context.TODO()))

		if !uselessCreate {
			require.Equal(t, capacity, atomic.LoadUint32(&count))
		}
	}

	testF(1, func(pool *WorkerPool) {
		pool.Submit(context.TODO(), func(context.Context) { time.Sleep(time.Millisecond) })
	}, false)
	testF(2, func(pool *WorkerPool) {
		f := func(context.Context) { time.Sleep(time.Millisecond) }
		pool.SubmitConcurrentDependent(context.TODO(), f, f)
	}, false)
	testF(1, func(pool *WorkerPool) {
		pool.Submit(context.TODO(), func(context.Context) { time.Sleep(time.Millisecond) })
	}, true)
	testF(2, func(pool *WorkerPool) {
		f := func(context.Context) { time.Sleep(time.Millisecond) }
		pool.SubmitConcurrentDependent(context.TODO(), f, f)
	}, true)
}

func TestWorkerPool_OptionWaitIfNoWorkersAvailableWithIdleTimeout(t *testing.T) {
	t.Parallel()

	opts := Options{
		Capacity:                 1,
		IdleTimeout:              1 * time.Millisecond,
		WaitIfNoWorkersAvailable: true,
	}
	p := New(opts)

	for i := 0; i < 100; i++ {
		err := p.Submit(context.TODO(), emptyFunc)
		require.Nil(t, err)
		time.Sleep(opts.IdleTimeout / time.Duration(i%2+1))
	}
	require.Nil(t, p.WaitDone(context.TODO()))
}

func TestWorkerPool_OptionCreateIfNoWorkersAvailable(t *testing.T) {
	t.Parallel()

	testF := func(capacity uint32, gof func(pool *WorkerPool)) {
		pool := New(Options{
			Capacity:                   capacity,
			IdleTimeout:                300 * time.Millisecond,
			CreateIfNoWorkersAvailable: true,
		})
		pool.Submit(context.TODO(), func(context.Context) { time.Sleep(80 * time.Millisecond) })
		donec := make(chan struct{})
		go func() {
			gof(pool)
			close(donec)
		}()
		select {
		case <-time.After(66 * time.Millisecond):
			t.Fatalf("timed out")
		case <-donec:
		}
		require.Nil(t, pool.WaitDone(context.TODO()))
	}

	f := func(context.Context) { time.Sleep(time.Millisecond) }
	testF(1, func(pool *WorkerPool) {
		pool.Submit(context.TODO(), f)
		pool.Submit(context.TODO(), f)
	})
	testF(1, func(pool *WorkerPool) {
		pool.Submit(context.TODO(), func(context.Context) { time.Sleep(time.Millisecond) })
	})
	testF(1, func(pool *WorkerPool) {
		pool.SubmitConcurrentDependent(context.TODO(), f, f)
	})
	testF(2, func(pool *WorkerPool) {
		pool.SubmitConcurrentDependent(context.TODO(), f, f)
	})
}

func TestWorkerPool_OptionCreateWorkerID(t *testing.T) {
	t.Parallel()

	pool := New(Options{
		Capacity:                 3,
		IdleTimeout:              300 * time.Millisecond,
		WaitIfNoWorkersAvailable: true,
		CreateWorkerID:           true,
	})

	ids := map[uint32]bool{}
	lock := sync.Mutex{}
	f := func(ctx context.Context) {
		id, _ := WorkerID(ctx)
		lock.Lock()
		ids[id] = true
		lock.Unlock()
	}
	for i := 0; i < 30; i++ {
		err := pool.Submit(context.TODO(), f)
		require.Nil(t, err)
	}
	require.Nil(t, pool.WaitDone(context.TODO()))

	require.Equal(t, 3, len(ids))
	for i := uint32(1); i <= 3; i++ {
		delete(ids, i)
	}
	require.Equal(t, 0, len(ids))
}

func TestWorkerPool_CorrectnessConcurrently(t *testing.T) {
	t.Parallel()

	for _, suite := range []struct {
		name string
		opts Options
	}{
		{
			name: "NoOptions",
			opts: Options{},
		},
		{
			name: "WithCapacity",
			opts: Options{Capacity: 100},
		},
		{
			name: "WithIdleTimeout",
			opts: Options{Capacity: 123, IdleTimeout: 56 * time.Millisecond},
		},
		{
			name: "WithResetInterval",
			opts: Options{
				Capacity:      123,
				IdleTimeout:   56 * time.Millisecond,
				ResetInterval: 100 * time.Millisecond,
			},
		},
		{
			name: "WaitIfNoWorkersAvailable",
			opts: Options{
				Capacity:                 128,
				IdleTimeout:              56 * time.Millisecond,
				ResetInterval:            100 * time.Millisecond,
				WaitIfNoWorkersAvailable: true,
			},
		},
		{
			name: "CreateIfNoWorkersAvailable",
			opts: Options{
				Capacity:                   32,
				IdleTimeout:                56 * time.Millisecond,
				ResetInterval:              100 * time.Millisecond,
				CreateIfNoWorkersAvailable: true,
			},
		},
		{
			name: "CreateWorkerID",
			opts: Options{
				Capacity:                 88,
				IdleTimeout:              56 * time.Millisecond,
				ResetInterval:            100 * time.Millisecond,
				WaitIfNoWorkersAvailable: true,
				CreateWorkerID:           true,
			},
		},
	} {
		opts := suite.opts
		t.Run(suite.name, func(t *testing.T) {
			t.Parallel()

			pool := New(opts)
			sum := int64(0)
			wg := sync.WaitGroup{}
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					for j := i * 800; j < (i+1)*800; j++ {
						num := int64(j)
						for {
							err := pool.Submit(context.Background(), func(context.Context) {
								atomic.AddInt64(&sum, num)
							})
							if err == nil {
								break
							}
						}
					}
				}(i)
			}
			wg.Wait()
			err := pool.WaitDone(context.Background())
			require.Nil(t, err)
			time.Sleep(100 * time.Millisecond)
			require.Equal(t, int64(7999*4000), atomic.LoadInt64(&sum))
		})
	}
}

func TestWorkerPool_RandomConcurrently(t *testing.T) {
	t.Parallel()

	testF := func(_ *testing.T, opts Options, submitF func(ctx context.Context, pool *WorkerPool, f Func)) {
		pool := New(opts)
		wg := sync.WaitGroup{}
		submitLoop := func() {
			for i := 0; i < 333; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()

					timeout1 := time.Duration(rand.Intn(50)) * time.Millisecond
					ctx, cancel := context.WithTimeout(context.Background(), timeout1)
					defer cancel()
					timeout2 := time.Duration(rand.Intn(50)) * time.Millisecond
					submitF(ctx, pool, func(context.Context) { time.Sleep(timeout2) })
				}()
			}
		}
		submitLoop()
		wg.Wait()
		time.Sleep(time.Duration(rand.Intn(60)) * time.Millisecond)
		submitLoop()
		wg.Wait()
		pool.WaitDone(context.TODO())
	}

	t.Run("NoOptions", func(t *testing.T) {
		t.Parallel()

		testF(t, Options{
			Capacity:    20,
			IdleTimeout: 55 * time.Millisecond,
		}, func(ctx context.Context, pool *WorkerPool, f Func) {
			pool.Submit(ctx, f)
		})
		testF(t, Options{
			Capacity:    20,
			IdleTimeout: 55 * time.Millisecond,
		}, func(ctx context.Context, pool *WorkerPool, f Func) {
			pool.SubmitConcurrentDependent(ctx, f)
		})
	})

	t.Run("WaitIfNoWorkersAvailable", func(t *testing.T) {
		t.Parallel()

		testF(t, Options{
			Capacity:                 66,
			IdleTimeout:              55 * time.Millisecond,
			WaitIfNoWorkersAvailable: true,
			CreateWorkerID:           true,
		}, func(ctx context.Context, pool *WorkerPool, f Func) {
			pool.Submit(ctx, f)
		})
		testF(t, Options{
			Capacity:                 66,
			IdleTimeout:              55 * time.Millisecond,
			WaitIfNoWorkersAvailable: true,
			CreateWorkerID:           true,
		}, func(ctx context.Context, pool *WorkerPool, f Func) {
			pool.SubmitConcurrentDependent(ctx, f)
		})
	})

	t.Run("CreateIfNoWorkersAvailable", func(t *testing.T) {
		t.Parallel()

		testF(t, Options{
			Capacity:                   20,
			IdleTimeout:                55 * time.Millisecond,
			CreateIfNoWorkersAvailable: true,
			CreateWorkerID:             true,
		}, func(ctx context.Context, pool *WorkerPool, f Func) {
			pool.Submit(ctx, f)
		})
		testF(t, Options{
			Capacity:                   20,
			IdleTimeout:                55 * time.Millisecond,
			CreateIfNoWorkersAvailable: true,
		}, func(ctx context.Context, pool *WorkerPool, f Func) {
			pool.SubmitConcurrentDependent(ctx, f)
		})
	})
}

func TestIDPool(t *testing.T) {
	p := newIDPool(false)
	for i := 0; i < 100; i++ {
		id := p.get()
		require.Equal(t, uint32(0), id)
		p.put(id)
	}

	p = newIDPool(true)
	for i := 1; i < 1000; i++ {
		require.Equal(t, uint32(i), p.get())
	}
	require.Equal(t, uint32(1000), p.next)

	ids := map[uint32]bool{}
	for i := 2; i < 100; i *= 2 {
		ids[uint32(i)] = true
		p.put(uint32(i))
	}
	for i := 2; i < 100; i *= 2 {
		delete(ids, p.get())
	}
	require.Equal(t, 0, len(ids))
	require.Equal(t, uint32(1000), p.next)

	require.Equal(t, uint32(1000), p.get())
	p.put(1000)
	require.Equal(t, uint32(1000), p.next)

	require.Equal(t, uint32(1000), p.get())
	require.Equal(t, uint32(1001), p.get())
	p.put(1000)
	require.Equal(t, uint32(1002), p.next)

	for _, id := range []uint32{0, 1000, p.next} {
		func(id uint32) {
			defer func() {
				e := recover()
				require.Contains(t, fmt.Sprintf("%v", e), "invalid id")
			}()
			p.put(id)
		}(id)
	}
}

func TestWorkerQueue(t *testing.T) {
	q := &workerQueue{}

	var workers []*worker
	for i := 0; i < 2048; i++ {
		w := &worker{id: uint32(i)}
		workers = append(workers, w)
		q.put(w)
		require.Equal(t, w, q.workers[i])
	}
	require.Equal(t, len(workers), q.length())
	require.Equal(t, workers, q.workers)

	for i, w := range workers {
		require.True(t, q.length() > 0)
		require.Equal(t, w, q.get(), i)
		if i < 32 {
			q.put(w)
		}
	}
	require.Equal(t, workers[:32], q.workers[:q.length()])

	index := q.binsearch(func(w *worker) bool {
		return w.id >= 20
	})
	require.Equal(t, 20, index)
	buffer := make([]*worker, 0, 16)
	q.movebefore(index, &buffer)
	require.Equal(t, workers[:20], buffer)
	require.Equal(t, 20, q.head)
	require.Equal(t, 32, q.tail)
	require.Equal(t, 32-len(buffer), q.length())
	q.movebefore(q.length(), &buffer)
	require.Equal(t, workers[20:32], buffer)
	require.Equal(t, 0, q.length())

	require.Panics(t, func() { _ = q.get() })

	q.clear()
	require.Equal(t, &workerQueue{}, q)

	q.workers = make([]*worker, 128, 128)
	q.cap = 128
	q.head = 128 - 34
	q.tail = 128 - 73 - 1
	q.count = 128 - 73 + 34
	copy(q.workers, workers[:128][73:])
	copy(q.workers[q.head:], workers[:128][:34])
	index = q.binsearch(func(w *worker) bool { return w.id > 129 })
	require.Equal(t, q.count, index)
}

func TestWorkerList(t *testing.T) {
	l := &workerList{}
	var workers []*worker
	n := 20
	for i := 0; i < n; i++ {
		workers = append(workers, &worker{})
	}
	for i, w := range workers {
		l.pushback(w)
		require.Equal(t, w, l.dummy.prev, strconv.Itoa(i))
	}
	for l.length() > 0 {
		i := n - l.length()
		require.Equal(t, workers[i], l.popfront(), strconv.Itoa(i))
	}
	for i, w := range workers {
		l.pushback(w)
		require.Equal(t, w, l.dummy.prev, strconv.Itoa(i))
	}

	require.Equal(t, workers, l.clear())
	newl := &workerList{}
	newl.pushback(&worker{})
	newl.popfront()
	require.Equal(t, newl, l)
}

func TestWaiterList(t *testing.T) {
	l := &waiterList{}
	var waiters []*waiter
	n := 20
	for i := 0; i < n; i++ {
		waiters = append(waiters, newWaiter())
	}
	for i, w := range waiters {
		l.pushback(w)
		require.Equal(t, w, l.dummy.prev, strconv.Itoa(i))
	}
	for l.length() > 0 {
		i := n - l.length()
		require.Equal(t, waiters[i], l.popfront(), strconv.Itoa(i))
	}

	l.clear()
	newl := &waiterList{}
	newl.pushback(&waiter{})
	newl.popfront()
	require.Equal(t, newl, l)
}

func BenchmarkGoroutines(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		wg := sync.WaitGroup{}
		for pb.Next() {
			wg.Add(1)
			go func() {
				defer wg.Done()
			}()
		}
		wg.Wait()
	})
}

func BenchmarkGoroutinesWithPool(b *testing.B) {
	p := New(Options{
		Capacity:                 100,
		IdleTimeout:              1 * time.Second,
		WaitIfNoWorkersAvailable: true,
	})
	ctx := context.TODO()
	defer p.WaitDone(ctx)
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		wg := sync.WaitGroup{}
		for pb.Next() {
			wg.Add(1)
			err := p.Submit(ctx, func(context.Context) {
				defer wg.Done()
			})
			if err != nil {
				panic(err)
			}
		}
		wg.Wait()
	})
}
