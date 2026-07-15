package proxy

import "github.com/ratecap/core/limiter"

type Priority = limiter.Priority

const (
	Sheddable = limiter.Sheddable
	Critical  = limiter.Critical
)

func ResolvePriority(headerValue string, defaultPriority Priority) Priority {
	switch headerValue {
	case "critical":
		return Critical
	case "sheddable":
		return Sheddable
	default:
		return defaultPriority
	}
}
