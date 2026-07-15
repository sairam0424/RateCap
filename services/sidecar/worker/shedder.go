package worker

import "sync/atomic"

type Shedder struct {
	inflight atomic.Int64
	max      int64
}

func NewShedder(max int64) *Shedder {
	return &Shedder{max: max}
}

func (s *Shedder) Allow() bool {
	if s.inflight.Load() >= s.max {
		return false
	}
	s.inflight.Add(1)
	return true
}

func (s *Shedder) Release() {
	s.inflight.Add(-1)
}
