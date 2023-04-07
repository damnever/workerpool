## WorkerPool

[![Go Reference](https://pkg.go.dev/badge/github.com/damnever/workerpool.svg)](https://pkg.go.dev/github.com/damnever/workerpool)

This package offers a convenient and efficient worker(goroutine) pool solution,
featuring a straightforward concurrent pattern called "pipeline" for effortless integration and usage.

The WorkerPool is extremely useful when we facing "morestack" issue.
Also some options can enable us to do lockless operations under some circumstances by using the worker id.
