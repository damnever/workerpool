package workerpool

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

func ExampleWorkerPool() {
	p := New(Options{
		Capacity:                   10,
		IdleTimeout:                5 * time.Minute,
		CreateIfNoWorkersAvailable: true,
	})

	count := uint32(0)
	for i := 0; i < 100; i++ {
		n := uint32(i + 1)
		_ = p.Submit(context.TODO(), func(context.Context) {
			atomic.AddUint32(&count, n)
		})
	}
	_ = p.WaitDone(context.TODO())

	fmt.Println(count)

	// Output:
	// 5050
}

func ExampleWorkerPool_locklessOperation() {
	p := New(Options{
		Capacity:                 8,
		WaitIfNoWorkersAvailable: true,
		CreateWorkerID:           true,
	})

	values := make([]uint32, 8, 8)
	for i := 0; i < 100; i++ {
		n := uint32(i + 1)
		_ = p.Submit(context.TODO(), func(ctx context.Context) {
			id, ok := WorkerID(ctx)
			if !ok {
				panic("not possible")
			}
			// The worker id starts with 1.
			values[id-1] += n
			time.Sleep(10 * time.Millisecond) // Too fast, sleep for a while..
		})
	}
	_ = p.WaitDone(context.TODO())

	sum := uint32(0)
	count := 0
	for _, v := range values {
		if v > 0 {
			count++
		}
		sum += v
	}
	fmt.Println(count)
	fmt.Println(sum)

	// Output:
	// 8
	// 5050
}

func ExamplePipeline() {
	pool := New(Options{
		Capacity:                 4,
		WaitIfNoWorkersAvailable: true,
	})

	pipeline := NewPipelineWith[int, int](PipelineOptions{
		FeederAsyncExecutor: GoSpawn,
		WorkerAsyncExecutor: pool.Submit,
	})
	inputs := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	_ = pipeline.StartFeeder(context.Background(), inputs)
	_ = pipeline.StartWorkerN(context.Background(), 4, func(_ context.Context, i int) int {
		return i * 2
	})

	sum := 0
	for v := range pipeline.Join() {
		sum += v
	}
	fmt.Println("zero canceled:", len(inputs) == pipeline.ProcessedCount())
	fmt.Println("sum:", sum)
	_ = pool.WaitDone(context.TODO()) // Clean up.

	// Output:
	// zero canceled: true
	// sum: 272
}

func ExampleWrap() {
	pool := New(Options{
		Capacity:                 8,
		WaitIfNoWorkersAvailable: true,
	})

	increase := func(a int) int {
		return a + 1
	}
	wrappedIncrease := Wrap(pool, func(_ context.Context, i int) (int, error) {
		return increase(i), nil
	})

	count := 0
	for i := 0; i < 100; i++ {
		count, _ = wrappedIncrease(context.TODO(), count)
	}
	_ = pool.WaitDone(context.TODO())
	fmt.Println(count)

	// Output:
	// 100
}
