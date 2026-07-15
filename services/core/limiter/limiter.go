package limiter

import "context"

type Action int

const (
	ALLOW Action = iota
	REJECT_429
	REJECT_503
	SHADOW_LOG
)

type Priority int

const (
	Sheddable Priority = iota
	Critical
)

type TokenReservation struct {
	Key   string
	Token string
}

type Decision struct {
	Action       Action
	RetryAfterMs int64
	Reservations []TokenReservation
}

type Request struct {
	Key              string
	Cost             int
	SkipReservations bool
	Priority         Priority
}

type Limiter interface {
	Check(ctx context.Context, req Request) (Decision, error)
}
