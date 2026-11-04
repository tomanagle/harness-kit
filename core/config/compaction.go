package config

import (
	"errors"
	"math"
	"time"
)

// CompactionSettings controls additional host-requested compaction before a
// prompt. Nil thresholds leave timing to the native runtime. Neither setting
// disables a runtime's own automatic compaction. Either threshold can trigger.
type CompactionSettings struct {
	AutoAtTokens  *int64   `json:"autoAtTokens"`
	AutoAtPercent *float64 `json:"autoAtPercent"`
	TimeoutMs     int64    `json:"timeoutMs"`
}

func CompactionDefaults() CompactionSettings { return CompactionSettings{TimeoutMs: 120000} }

func (s CompactionSettings) Clone() CompactionSettings {
	if s.AutoAtTokens != nil {
		n := *s.AutoAtTokens
		s.AutoAtTokens = &n
	}
	if s.AutoAtPercent != nil {
		n := *s.AutoAtPercent
		s.AutoAtPercent = &n
	}
	return s
}

func (s CompactionSettings) Validate() error {
	if s.TimeoutMs < 1 || s.TimeoutMs > math.MaxInt64/int64(time.Millisecond) {
		return errors.New("compaction.timeoutMs must be a positive supported duration")
	}
	if s.AutoAtTokens != nil && *s.AutoAtTokens < 1 {
		return errors.New("compaction.autoAtTokens must be positive or null")
	}
	if n := s.AutoAtPercent; n != nil && (math.IsNaN(*n) || math.IsInf(*n, 0) || *n <= 0 || *n > 100) {
		return errors.New("compaction.autoAtPercent must be greater than 0 and at most 100, or null")
	}
	return nil
}
