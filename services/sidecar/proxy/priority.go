package proxy

type Priority int

const (
	Sheddable Priority = iota
	Critical
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
