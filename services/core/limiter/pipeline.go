package limiter

import "context"

type Pipeline struct {
	tiers []Limiter
}

func NewPipeline(tiers ...Limiter) *Pipeline {
	return &Pipeline{tiers: tiers}
}

func (p *Pipeline) Check(ctx context.Context, req Request) (Decision, error) {
	final := Decision{Action: ALLOW}
	for _, tier := range p.tiers {
		d, err := tier.Check(ctx, req)
		if err != nil || d.Action != ALLOW {
			return d, err
		}
		if d.Token != "" {
			final.Token = d.Token
		}
	}
	return final, nil
}
