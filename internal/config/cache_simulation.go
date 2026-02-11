// Package config provides cache simulation configuration with hot-reload support.
package config

import (
	"math/rand"
	"sync/atomic"
)

// Default cache simulation parameters.
const (
	DefaultCacheSimReadRatioMin     = 0.80
	DefaultCacheSimReadRatioMax     = 0.95
	DefaultCacheSimCreationRatioMin = 0.005
	DefaultCacheSimCreationRatioMax = 0.015
	DefaultCacheSimMinInputTokens   = int64(1024)
)

// CacheSimValues holds resolved (non-zero) cache simulation values.
type CacheSimValues struct {
	Enabled          bool
	ReadRatioMin     float64
	ReadRatioMax     float64
	CreationRatioMin float64
	CreationRatioMax float64
	MinInputTokens   int64
}

var globalCacheSim atomic.Value // stores *CacheSimValues

func init() {
	// Initialize with defaults
	globalCacheSim.Store(&CacheSimValues{
		Enabled:          true,
		ReadRatioMin:     DefaultCacheSimReadRatioMin,
		ReadRatioMax:     DefaultCacheSimReadRatioMax,
		CreationRatioMin: DefaultCacheSimCreationRatioMin,
		CreationRatioMax: DefaultCacheSimCreationRatioMax,
		MinInputTokens:   DefaultCacheSimMinInputTokens,
	})
}

// UpdateCacheSimulation updates the global cache simulation config.
// Called when config is loaded or hot-reloaded.
func UpdateCacheSimulation(cfg CacheSimulationConfig) {
	v := &CacheSimValues{
		Enabled:          true,
		ReadRatioMin:     DefaultCacheSimReadRatioMin,
		ReadRatioMax:     DefaultCacheSimReadRatioMax,
		CreationRatioMin: DefaultCacheSimCreationRatioMin,
		CreationRatioMax: DefaultCacheSimCreationRatioMax,
		MinInputTokens:   DefaultCacheSimMinInputTokens,
	}
	if cfg.Enabled != nil {
		v.Enabled = *cfg.Enabled
	}
	if cfg.ReadRatioMin > 0 {
		v.ReadRatioMin = cfg.ReadRatioMin
	}
	if cfg.ReadRatioMax > 0 {
		v.ReadRatioMax = cfg.ReadRatioMax
	}
	if cfg.CreationRatioMin > 0 {
		v.CreationRatioMin = cfg.CreationRatioMin
	}
	if cfg.CreationRatioMax > 0 {
		v.CreationRatioMax = cfg.CreationRatioMax
	}
	if cfg.MinInputTokens > 0 {
		v.MinInputTokens = cfg.MinInputTokens
	}
	// Sanity: ensure min <= max
	if v.ReadRatioMin > v.ReadRatioMax {
		v.ReadRatioMin, v.ReadRatioMax = v.ReadRatioMax, v.ReadRatioMin
	}
	if v.CreationRatioMin > v.CreationRatioMax {
		v.CreationRatioMin, v.CreationRatioMax = v.CreationRatioMax, v.CreationRatioMin
	}
	globalCacheSim.Store(v)
}

// GetCacheSimulation returns the current cache simulation config.
// Safe for concurrent use.
func GetCacheSimulation() *CacheSimValues {
	return globalCacheSim.Load().(*CacheSimValues)
}

// CacheSimReadRatio returns a random cache read ratio based on current config.
func CacheSimReadRatio(inputTokens int64) float64 {
	v := GetCacheSimulation()
	jitter := rand.Float64()
	ratio := v.ReadRatioMin + jitter*(v.ReadRatioMax-v.ReadRatioMin)
	if inputTokens > 50000 {
		ratio += 0.03
		if ratio > 0.97 {
			ratio = 0.97
		}
	}
	return ratio
}

// CacheSimCreationRatio returns a random cache creation ratio based on current config.
func CacheSimCreationRatio() float64 {
	v := GetCacheSimulation()
	jitter := rand.Float64()
	return v.CreationRatioMin + jitter*(v.CreationRatioMax-v.CreationRatioMin)
}
