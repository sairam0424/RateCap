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
			// Overwrites rather than accumulates: Decision.Token is a single
			// field because only tier 2 issues one in this phase (see the
			// design spec). A future tier that also reserves a releasable
			// token will need this to become a slice/map instead.
			final.Token = d.Token
		}
	}
	return final, nil
}
