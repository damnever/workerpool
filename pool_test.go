package workerpool

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func emptyFunc(context.Context) {}

func TestWorkerPool_OptionCapacity(t *testing.T) {
	t.Parallel()

	f := func(context.Context) { time.Sleep(10 * time.Millisecond) }
	{
		pool := New(Options{Capacity: 1})
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		require.Nil(t, pool.Submit(ctx, func(context.Context) { time.Sleep(100 * time.Millisecond) }))
		require.Equal(t, ErrNoWorkersAvaiable, pool.Submit(ctx, emptyFunc))
		require.Nil(t, pool.WaitDone(context.TODO()))
	}
	{
		pool := New(Options{Capacity: 2})
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		require.Nil(t, pool.SubmitConcurrentDependent(ctx, f, f))
		require.Equal(t, ErrNoWorkersAvaiable, pool.SubmitConcurrentDependent(ctx, emptyFunc))
		require.Nil(t, pool.WaitDone(context.TODO()))
	}
	{
		pool := New(Options{Capacity: 1})
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		require.Nil(t, pool.SubmitConcurrentDependent(ctx, emptyFunc))
		require.Equal(t, ErrNoWorkersAvaiable, pool.SubmitConcurrentDependent(ctx, f, f))
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
	case <-time.After(300 * time.Millisecond):
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
		case <-time.After(100 * time.Millisecond):
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
		case <-time.After(33 * time.Millisecond):
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

func TestWorkerPool_Concurrent(t *testing.T) {
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
	p := newIDPool()
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

func TestCapacityNotifier(t *testing.T) {
	t.Parallel()

	n := capacityNotifier{}
	{
		n.incr()
		n.decr()
		for i := 0; i < 100; i++ {
			select {
			case <-n.availabled():
				t.Fatal("should block forever")
			default:
			}
		}
	}
	{
		stopc := make(chan struct{})
		(&n).enable(stopc, 1)
		select {
		case <-n.availabled():
		case <-time.After(30 * time.Millisecond):
			t.Fatal("should be available")
		}
		n.decr()
		select {
		case <-n.availabled():
			t.Fatal("should be blocked")
		case <-time.After(30 * time.Millisecond):
		}
		n.incr()
		select {
		case <-n.availabled():
		case <-time.After(30 * time.Millisecond):
			t.Fatal("should be available")
		}
		close(stopc)
		n.decr()
		n.decr()
		n.incr()
	}
}

func BenchmarkWorkerPool(b *testing.B) {
	p := New(Options{
		Capacity:                   20,
		IdleTimeout:                5 * time.Minute,
		CreateIfNoWorkersAvailable: true,
	})

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = p.Submit(context.TODO(), func(context.Context) {
		})
	}
	_ = p.WaitDone(context.TODO())
}
