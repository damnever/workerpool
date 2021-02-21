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
