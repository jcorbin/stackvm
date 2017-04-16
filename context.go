package stackvm

import "errors"

var errRunQFull = errors.New("run queue full")

// Handler is implemented to handle multiple results during a machine run;
// without a handler being set, any fork operation will fail.
type Handler interface {
	Handle(*Mach) error
}

type context interface {
	Handler
	queue(*Mach) error
	next() *Mach
}

// runq implements a capped lifo queue
type runq struct {
	context
	q []*Mach
}

func newRunq(ctx context, n int) *runq {
	return &runq{ctx, make([]*Mach, 0, n)}
}

func (rq *runq) queue(m *Mach) error {
	if len(rq.q) == cap(rq.q) {
		return errRunQFull
	}
	rq.q = append(rq.q, m)
	return nil
}

func (rq *runq) next() *Mach {
	if len(rq.q) == 0 {
		return nil
	}
	i := len(rq.q) - 1
	m := rq.q[i]
	rq.q = rq.q[:i]
	return m
}

type handler func(*Mach) error

func (f handler) Handle(m *Mach) error { return f(m) }
func (f handler) queue(*Mach) error    { return errNoQueue }
func (f handler) next() *Mach          { return nil }

var defaultContext = _defaultContext{}

type _defaultContext struct{}

func (dc _defaultContext) Handle(m *Mach) error { return m.Err() }
func (dc _defaultContext) queue(*Mach) error    { return errNoQueue }
func (dc _defaultContext) next() *Mach          { return nil }
