package streamtypebalancer

import (
	"math"
)

type slopescorer struct {
	max_score float64
}

func newSlopescorer() *slopescorer {
	return &slopescorer{}
}

func (s *slopescorer) score(val float64) float64 {
	if math.IsNaN(val) {
		return 0
	}
	s.max_score = max(s.max_score*0.99, val, -val)
	normalized := val / s.max_score
	return scoreFunction(normalized)
}

func scoreFunction(x float64) float64 {
	return (x*x*x + x/3) / (4. / 3) // normalize to maximum 1
}
