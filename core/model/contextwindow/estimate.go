package contextwindow

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/tomanagle/harness-kit/core/model"
)

// ApproximateJSON explicitly estimates JSON bytes/bytesPerToken. It is useful
// for coarse guardrails, never an exact model token or multimodal quota count.
func ApproximateJSON(bytesPerToken float64) Estimator {
	return func(ctx context.Context, r model.Request) (Estimate, error) {
		if err := ctx.Err(); err != nil {
			return Estimate{}, err
		}
		if bytesPerToken <= 0 || math.IsNaN(bytesPerToken) || math.IsInf(bytesPerToken, 0) {
			return Estimate{}, errors.New("contextwindow: bytesPerToken must be finite and positive")
		}
		b, err := json.Marshal(r)
		if err != nil {
			return Estimate{}, err
		}
		n := math.Ceil(float64(len(b)) / bytesPerToken)
		if n > math.MaxInt64 {
			return Estimate{}, errors.New("contextwindow: estimate overflow")
		}
		return Estimate{Tokens: int64(n), Exact: false}, nil
	}
}
