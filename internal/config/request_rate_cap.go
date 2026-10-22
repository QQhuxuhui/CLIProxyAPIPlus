package config

import (
	"fmt"
	"math"
	"time"
)

// RequestRateCapConfig controls adaptive per-credential generation admission.
type RequestRateCapConfig struct {
	Enabled          bool    `yaml:"enabled" json:"enabled"`
	FailFast         bool    `yaml:"fail-fast" json:"fail-fast"`
	InitialPerMinute int     `yaml:"initial-per-minute" json:"initial-per-minute"`
	MinPerMinute     int     `yaml:"min-per-minute" json:"min-per-minute"`
	MaxPerMinute     int     `yaml:"max-per-minute" json:"max-per-minute"`
	ProbeInterval    string  `yaml:"probe-interval" json:"probe-interval"`
	Headroom         float64 `yaml:"headroom" json:"headroom"`
}

func (c RequestRateCapConfig) WithDefaults() RequestRateCapConfig {
	if c.InitialPerMinute == 0 {
		c.InitialPerMinute = 20
	}
	if c.MinPerMinute == 0 {
		c.MinPerMinute = 5
	}
	if c.MaxPerMinute == 0 {
		c.MaxPerMinute = 60
	}
	if c.ProbeInterval == "" {
		c.ProbeInterval = "5m"
	}
	if c.Headroom == 0 {
		c.Headroom = 1
	}
	return c
}

func (c RequestRateCapConfig) Validate() error {
	c = c.WithDefaults()
	if c.MinPerMinute < 1 || c.InitialPerMinute < c.MinPerMinute || c.MaxPerMinute < c.InitialPerMinute || c.MaxPerMinute > 1000000 {
		return fmt.Errorf("request rate cap requires 1 <= min-per-minute <= initial-per-minute <= max-per-minute <= 1000000")
	}
	if math.IsNaN(c.Headroom) || math.IsInf(c.Headroom, 0) || c.Headroom <= 0 || c.Headroom > 1 {
		return fmt.Errorf("request rate cap headroom must be in (0, 1]")
	}
	if d, err := time.ParseDuration(c.ProbeInterval); err != nil || d <= 0 {
		return fmt.Errorf("request rate cap probe-interval must be a positive duration")
	}
	if c.FailFast && !c.Enabled {
		return fmt.Errorf("request rate cap fail-fast requires enabled")
	}
	return nil
}
