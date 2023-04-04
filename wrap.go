package workerpool

import (
	"context"
	"sync"
)

// ParameterizedFunc is the type of function with input and output parameters.
type ParameterizedFunc[In, Out any] func(context.Context, In) (Out, error)

// NoParameter is a placeholder for convinience.
type NoParameter struct{}

type wrapResult[T any] struct {
	out T
	err error
}

// Wrap wraps a function for ease of future use,
// allowing the wrapped function to be executed within the WorkerPool.
func Wrap[In, Out any](p *WorkerPool, f ParameterizedFunc[In, Out]) ParameterizedFunc[In, Out] {
	chanPool := sync.Pool{}

	return func(ctx context.Context, in In) (Out, error) {
		c, ok := chanPool.Get().(chan wrapResult[Out])
		if !ok {
			c = make(chan wrapResult[Out], 1)
		}

		err := p.Submit(ctx, func(innerctx context.Context) {
			out, err := f(innerctx, in)
			c <- wrapResult[Out]{out: out, err: err}
		})
		if err != nil {
			chanPool.Put(c)
			var out Out
			return out, err
		}
		ret := <-c
		chanPool.Put(c)
		return ret.out, ret.err
	}
}
