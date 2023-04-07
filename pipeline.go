package workerpool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

var (
	// ErrPipelineFrozen means the pipeline does not accept any further operations
	// since Pipeline.Join has been called.
	ErrPipelineFrozen = fmt.Errorf("workerpool: pipeline is frozed")
)

// AsyncExecutor is a function type used for executing a function asynchronously.
type AsyncExecutor func(ctx context.Context, fn Func) error

// GoSpawn is an implementation of AsyncExecutor that spawns a goroutine and directly
// executes the function (fn) within it.
func GoSpawn(ctx context.Context, fn Func) error {
	go fn(ctx)
	return nil
}

// PipelineOptions configure the Pipeline.
//
// NOTE that if you enable the WaitIfNoWorkersAvailable option
// with a small Capacity and BufferSize while using the WorkerPool
// for both Feeder and Worker, it may result in a deadlock.
// Additionally, chaining multiple Pipelines under such circumstances
// may also cause a deadlock.
type PipelineOptions struct {
	// FeederAsyncExecutor is the AsyncExecutor used by feeder.
	FeederAsyncExecutor AsyncExecutor
	// WorkerAsyncExecutor is the AsyncExecutor used by worker.
	WorkerAsyncExecutor AsyncExecutor
	// InputBufferSize is the buffer size of input channel.
	InputBufferSize int
	// OutputBufferSize is the buffer size of output channel.
	OutputBufferSize int
}

// Pipeline utilizes an input and an output channel to create a concurrent processor with three stages.
type Pipeline[In, Out any] struct {
	feederGo AsyncExecutor
	workerGo AsyncExecutor
	feederWg sync.WaitGroup
	workerWg sync.WaitGroup
	inputc   chan In
	outputc  chan Out

	processed atomic.Uint32
	joined    atomic.Bool
}

// NewPipeline creates a new pipeline that utilizes fire-and-forget goroutines.
func NewPipeline[In, Out any]() *Pipeline[In, Out] {
	return NewPipelineWith[In, Out](PipelineOptions{
		FeederAsyncExecutor: GoSpawn,
		WorkerAsyncExecutor: GoSpawn,
	})
}

// NewPipelineWith creates a new Pipeline with PipelineOptions.
func NewPipelineWith[In, Out any](opts PipelineOptions) *Pipeline[In, Out] {
	if opts.FeederAsyncExecutor == nil {
		opts.FeederAsyncExecutor = GoSpawn
	}
	if opts.WorkerAsyncExecutor == nil {
		opts.WorkerAsyncExecutor = GoSpawn
	}
	return &Pipeline[In, Out]{
		feederGo: opts.FeederAsyncExecutor,
		workerGo: opts.WorkerAsyncExecutor,

		feederWg: sync.WaitGroup{},
		workerWg: sync.WaitGroup{},

		inputc:  make(chan In, opts.InputBufferSize),
		outputc: make(chan Out, opts.OutputBufferSize),

		processed: atomic.Uint32{},
		joined:    atomic.Bool{},
	}
}

// StartFeeder initiates the feeding process of an array of inputs within the AsyncExecutor.
// The feeding process can be interrupted by the context.Context without any signal.
//
// This method must be invoked prior to Join, failing which ErrPipelineFrozen will be returned.
func (p *Pipeline[In, Out]) StartFeeder(ctx context.Context, items []In) error {
	return p.StartFeederFunc(ctx, func(ctx context.Context, inc chan<- In) {
		for _, e := range items {
			select {
			case <-ctx.Done():
				return
			case inc <- e:
			}
		}
	})
}

// StartFeeder initiates a feeding process within the asynchronous executor.
// It's important for the caller to check the context.Context inside the feedLoop
// and ensure that the feeding process is stopped properly.
//
// This method must be invoked prior to Join, failing which ErrPipelineFrozen will be returned.
func (p *Pipeline[In, Out]) StartFeederFunc(ctx context.Context, feedLoop func(context.Context, chan<- In)) error {
	if p.joined.Load() {
		return ErrPipelineFrozen
	}

	p.feederWg.Add(1)
	return p.feederGo(ctx, func(ctx context.Context) {
		defer p.feederWg.Done()

		feedLoop(ctx, p.inputc)
	})
}

// StartWorkerN starts N workers to process inputs within the AsyncExecutor.
// The workers will stop if either the context is done or all inputs have been processed.
//
// This method must be invoked prior to Join, failing which ErrPipelineFrozen will be returned.
func (p *Pipeline[In, Out]) StartWorkerN(ctx context.Context, concurrency int, workOne func(context.Context, In) Out) error {
	for i := 0; i < concurrency; i++ {
		if err := p.StartWorker(ctx, workOne); err != nil {
			return err
		}
	}
	return nil
}

// StartWorker initiates a worker to process the inputs within the AsyncExecutor.
// The worker will stop if either the context is done or all inputs have been processed.
//
// This method must be invoked prior to Join, failing which ErrPipelineFrozen will be returned.
func (p *Pipeline[In, Out]) StartWorker(ctx context.Context, workOne func(context.Context, In) Out) error {
	if p.joined.Load() {
		return ErrPipelineFrozen
	}

	p.workerWg.Add(1)
	return p.workerGo(ctx, func(ctx context.Context) {
		defer p.workerWg.Done()

		for {
			select {
			case <-ctx.Done():
				return
			case in, ok := <-p.inputc:
				if !ok {
					return
				}
				output := workOne(ctx, in)
				p.processed.Add(1)
				p.outputc <- output
			}
		}
	})
}

// Join returns an output channel, the channel will be closed after all tasks are done.
// It is the caller's responsibility to check if all inputs are processed;
// the ProcessedCount variable serves this purpose.
// The pipeline is frozen after the join.
func (p *Pipeline[In, Out]) Join() <-chan Out {
	if !p.joined.CompareAndSwap(false, true) {
		return p.outputc
	}

	go func() {
		p.feederWg.Wait()
		close(p.inputc)
		p.workerWg.Wait()
		close(p.outputc)
	}()
	return p.outputc
}

// ProcessedCount keeps track of the number of inputs that have been processed.
// The count is stable if the output channel has been closed.
func (p *Pipeline[In, Out]) ProcessedCount() int {
	return int(p.processed.Load())
}
