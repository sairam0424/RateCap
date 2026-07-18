package limiter

import "context"

type Pipeline struct {
	tiers []Limiter
}

func NewPipeline(tiers ...Limiter) *Pipeline {
	return &Pipeline{tiers: tiers}
}

func (p *Pipeline) Check(ctx context.Context, req Request) (Decision, error) {
	var reserved []TokenReservation
	var lastTier string
	finalAction := ALLOW
	for _, tier := range p.tiers {
		d, err := tier.Check(ctx, req)
		reserved = append(reserved, d.Reservations...)
		if err != nil || (d.Action != ALLOW && d.Action != QUEUE) {
			d.Reservations = reserved
			return d, err
		}
		if d.Action == QUEUE {
			finalAction = QUEUE
		}
		if d.Tier != "" {
			lastTier = d.Tier
		}
	}
	return Decision{Action: finalAction, Reservations: reserved, Tier: lastTier}, nil
}
