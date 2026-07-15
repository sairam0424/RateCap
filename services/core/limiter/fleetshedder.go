package limiter

import (
	"context"
	"sync"
)

const fleetKey = "fleet"

type FleetShedder struct {
	store concurrencyChecker

	mu                  sync.RWMutex
	cap                 int
	reservedCriticalPct int
	maxDurationMs       int64
	shadowMode          bool
}

func NewFleetShedder(s concurrencyChecker, cap, reservedCriticalPct int, maxDurationMs int64, shadowMode bool) *FleetShedder {
	return &FleetShedder{store: s, cap: cap, reservedCriticalPct: reservedCriticalPct, maxDurationMs: maxDurationMs, shadowMode: shadowMode}
}

func (l *FleetShedder) Reconfigure(cap, reservedCriticalPct int, maxDurationMs int64, shadowMode bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = cap
	l.reservedCriticalPct = reservedCriticalPct
	l.maxDurationMs = maxDurationMs
	l.shadowMode = shadowMode
}

func (l *FleetShedder) Check(ctx context.Context, req Request) (Decision, error) {
	if req.SkipReservations {
		return Decision{Action: ALLOW}, nil
	}

	l.mu.RLock()
	cap, pct, maxDurationMs, shadowMode := l.cap, l.reservedCriticalPct, l.maxDurationMs, l.shadowMode
	l.mu.RUnlock()

	effectiveCap := cap
	if req.Priority != Critical {
		effectiveCap = cap * (100 - pct) / 100
	}

	allowed, token, err := l.store.IncrConcurrent(ctx, fleetKey, effectiveCap, maxDurationMs)
	if err != nil {
		return Decision{}, err
	}

	if allowed {
		return Decision{Action: ALLOW, Reservations: []TokenReservation{{Key: fleetKey, Token: token}}}, nil
	}

	if shadowMode {
		_, reservedToken, err := l.store.IncrConcurrent(ctx, fleetKey, unboundedCap, maxDurationMs)
		if err != nil {
			return Decision{}, err
		}
		return Decision{Action: SHADOW_LOG, Reservations: []TokenReservation{{Key: fleetKey, Token: reservedToken}}}, nil
	}

	return Decision{Action: REJECT_503}, nil
}
