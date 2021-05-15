## WorkerPool

[![Go Reference](https://pkg.go.dev/badge/github.com/damnever/workerpool.svg)](https://pkg.go.dev/github.com/damnever/workerpool)

[![codecov](https://codecov.io/gh/damnever/workerpool/branch/development/graph/badge.svg?token=I14YQ63Q6F)](https://codecov.io/gh/damnever/workerpool)

An implementation of worker(goroutine) pool.

It is extremely useful when we facing "morestack" issue.
Also some options can enable us to do lockless operations under some circumstances by using the worker id.
