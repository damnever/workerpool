package workerpool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWrap(t *testing.T) {
	t.Parallel()

	p := New(Options{
		Capacity:                 10,
		IdleTimeout:              100 * time.Millisecond,
		WaitIfNoWorkersAvailable: true,
	})
	t.Cleanup(func() { p.WaitDone(context.Background()) })

	f := Wrap(p, func(_ context.Context, i int) (int, error) {
		time.Sleep(time.Duration(i%10+1) * time.Millisecond)
		return i + 1, nil
	})

	t.Run("sequential", func(t *testing.T) {
		t.Parallel()

		expect := 0
		actual := 0
		for i := 0; i < 100; i++ {
			ctx := context.Background()
			if i%2 == 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			v, err := f(ctx, i)
			if i%2 == 0 {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
				expect += i + 1
			}
			actual += v
		}
		require.Equal(t, expect, actual)
	})

	t.Run("parallel", func(t *testing.T) {
		t.Parallel()

		expectAtomic := atomic.Int64{}
		actualAtomic := atomic.Int64{}
		wg := sync.WaitGroup{}
		for i := 0; i < 500; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()

				ctx := context.Background()
				if i%2 == 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				v, err := f(ctx, i)
				if i%2 == 0 {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.NoError(t, err)
					expectAtomic.Add(int64(i + 1))
				}
				actualAtomic.Add(int64(v))
			}(i)
		}
		wg.Wait()
		require.Equal(t, expectAtomic.Load(), actualAtomic.Load())
	})
}
