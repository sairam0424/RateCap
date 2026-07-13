package limiter

import "context"

type Action int

const (
	ALLOW Action = iota
	REJECT_429
	REJECT_503
	SHADOW_LOG
)

type Decision struct {
	Action       Action
	RetryAfterMs int64
}

type Request struct {
	Key  string
	Cost int
}

type Limiter interface {
	Check(ctx context.Context, req Request) (Decision, error)
}
