package proxy

import "github.com/sairam0424/RateCap/services/core/limiter"

type Priority = limiter.Priority

const (
	Sheddable = limiter.Sheddable
	Critical  = limiter.Critical
)

func ResolvePriority(headerValue string, routeMatched bool, defaultPriority Priority) Priority {
	switch headerValue {
	case "critical":
		return Critical
	case "sheddable":
		return Sheddable
	}
	if routeMatched {
		return Critical
	}
	return defaultPriority
}
