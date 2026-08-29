package worker

import (
	"math/rand"
	"sync/atomic"
)

type Shedder struct {
	inflight     atomic.Int64
	max          int64
	rampStartPct int64
}

func NewShedder(max int64) *Shedder {
	return NewShedderWithRamp(max, 100)
}

// NewShedderWithRamp starts probabilistically rejecting once inflight
// crosses rampStartPct% of max, linearly increasing the reject probability
// to 100% exactly at max — replacing a hard on/off cutoff (Stripe's
// documented flapping failure mode for binary shed curves) with a gradual
// one. rampStartPct=100 recovers the exact original hard-cutoff behavior.
func NewShedderWithRamp(max int64, rampStartPct int) *Shedder {
	return &Shedder{max: max, rampStartPct: int64(rampStartPct)}
}

func (s *Shedder) Allow() bool {
	for {
		current := s.inflight.Load()
		if current >= s.max {
			return false
		}
		if !s.shouldAdmitAtLoad(current) {
			return false
		}
		if s.inflight.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// shouldAdmitAtLoad returns false with a probability that ramps linearly
// from 0 at rampStartPct of max to (just under) 1 at max itself — current
// is always < s.max here (checked by the caller), so rejectProbability
// never reaches exactly 1 via this path alone.
func (s *Shedder) shouldAdmitAtLoad(current int64) bool {
	rampStart := s.max * s.rampStartPct / 100
	if current < rampStart {
		return true
	}
	rampWindow := s.max - rampStart
	if rampWindow <= 0 {
		return false
	}
	intoRamp := current - rampStart
	rejectProbability := float64(intoRamp) / float64(rampWindow)
	return rand.Float64() >= rejectProbability
}

func (s *Shedder) Release() {
	s.inflight.Add(-1)
}

func (s *Shedder) InFlight() int64 {
	return s.inflight.Load()
}
