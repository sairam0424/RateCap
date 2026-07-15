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
	for {
		current := s.inflight.Load()
		if current >= s.max {
			return false
		}
		if s.inflight.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (s *Shedder) Release() {
	s.inflight.Add(-1)
}
