package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type RateLimiterConfig struct {
	DefaultRate  int  `yaml:"default_rate"`
	DefaultBurst int  `yaml:"default_burst"`
	ShadowMode   bool `yaml:"shadow_mode"`
}

type Config struct {
	SyncRate int `yaml:"sync_rate"`
	Tiers    struct {
		RateLimiter RateLimiterConfig `yaml:"rate_limiter"`
	} `yaml:"tiers"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	return &cfg, nil
}
