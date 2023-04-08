package workerpool

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPipeline_AsyncExecutor(t *testing.T) {
	t.Parallel()

	makeExecutor := func() (*atomic.Int32, AsyncExecutor) {
		counter := &atomic.Int32{}
		return counter, func(ctx context.Context, fn Func) error {
			counter.Add(1)
			go fn(ctx)
			return nil
		}
	}
	feederCount, feederExecutor := makeExecutor()
	workerCount, workerExecutor := makeExecutor()

	pipeline := NewPipelineWith[int, int](PipelineOptions{
		FeederAsyncExecutor: feederExecutor,
		WorkerAsyncExecutor: workerExecutor,
	})

	require.NoError(t, pipeline.StartFeeder(context.Background(), []int{1, 2}))
	require.NoError(t, pipeline.StartFeeder(context.Background(), []int{3, 4}))
	require.NoError(t, pipeline.StartWorker(context.Background(), func(_ context.Context, i int) int {
		return i * 2
	}))
	require.NoError(t, pipeline.StartWorker(context.Background(), func(_ context.Context, i int) int {
		return i * 2
	}))
	require.NoError(t, pipeline.StartWorkerN(context.Background(), 2, func(_ context.Context, i int) int {
		return i * 2
	}))

	require.Equal(t, int32(2), feederCount.Load())
	require.Equal(t, int32(4), workerCount.Load())
	sum := 0
	for v := range pipeline.Join() {
		sum += v
	}
	require.Equal(t, 20, sum)
	require.Equal(t, 4, pipeline.ProcessedCount())
}
