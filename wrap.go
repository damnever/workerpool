package workerpool

import (
	"context"
	"sync"
)

// None is a placeholder for convinience if there is no parameters or no return value.
type None struct{}

type wrapResult[T any] struct {
	val T
	err error
}

// Wrap wraps a function for ease of future use,
// allowing the wrapped function to be executed within the WorkerPool.
func Wrap[In, Out any](p *WorkerPool, f func(context.Context, In) (Out, error)) func(context.Context, In) (Out, error) {
	chanPool := sync.Pool{}

	return func(ctx context.Context, in In) (Out, error) {
		c, ok := chanPool.Get().(chan wrapResult[Out])
		if !ok {
			c = make(chan wrapResult[Out], 1)
		}

		err := p.Submit(ctx, func(innerctx context.Context) {
			out, err := f(innerctx, in)
			c <- wrapResult[Out]{val: out, err: err}
		})
		if err != nil {
			chanPool.Put(c)
			var out Out
			return out, err
		}
		ret := <-c
		chanPool.Put(c)
		return ret.val, ret.err
	}
}
