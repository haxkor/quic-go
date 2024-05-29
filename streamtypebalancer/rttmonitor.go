package streamtypebalancer

import (
	"fmt"
	"math"
	"time"

	"github.com/quic-go/quic-go/internal/utils/ringbuffer"

	"gonum.org/v1/gonum/stat"
)

type RTTTuple struct {
	ts  time.Time
	rtt time.Duration
}

func (t *RTTTuple) toFloats() (x, y float64) {
	return float64(t.ts.UnixMilli()), float64(t.rtt)
}

type RTTMonitor struct {
	samples  ringbuffer.RingBuffer[RTTTuple]
	rttInput chan time.Duration

	debug_func func(name, msg string)

	timeframes []time.Duration

	RegressionResults []regressionResult

	inputCounter int

	slopescorer_short slopescorer
	slopescorer_long  slopescorer
}

func NewRTTMonitor(timeframes []time.Duration) *RTTMonitor {
	r := RTTMonitor{timeframes: timeframes}
	r.samples.Init(32)
	r.RegressionResults = make([]regressionResult, len(timeframes))
	r.rttInput = make(chan time.Duration)
	r.slopescorer_short = *newSlopescorer()
	r.slopescorer_long = *newSlopescorer()

	go r.loopaddSentDataActual()

	return &r
}

func (r *RTTMonitor) AddSample(sample time.Duration) {
	r.rttInput <- sample
}

func (r *RTTMonitor) addSampleActual() {
	rtt := <-r.rttInput
	r.samples.PushBack(RTTTuple{time.Now(), rtt})
}

func (r *RTTMonitor) loopaddSentDataActual() {
	for {
		r.addSampleActual()
	}
}

func (r *RTTMonitor) PopOld(more_than time.Duration, since time.Time) {
	for !r.samples.Empty() &&
		since.Sub(r.samples.PeekFront().ts) > more_than {
		r.samples.PopFront()
	}
}

func (r *RTTMonitor) RegressAll() {

	now := time.Now()
	r.PopOld(r.timeframes[0], now)

	samples := r.samples.Iter()

	regression_inputs := make([]regressionInput, len(r.timeframes))
	for _, r := range regression_inputs {
		r.init()
	}

	include_until := 0
	for _, sample := range samples {
		if include_until+1 < len(r.timeframes) &&
			now.Sub(sample.ts) < r.timeframes[include_until+1] {
			include_until++
		}
		x, y := sample.toFloats()
		for i := 0; i <= include_until; i++ {
			regression_inputs[i].X.PushBack(x)
			regression_inputs[i].Y.PushBack(y)
		}
	}

	for i, reg_input := range regression_inputs {
		a, b := stat.LinearRegression(reg_input.X.Iter(), reg_input.Y.Iter(), nil, false)
		r.RegressionResults[i].Offset, r.RegressionResults[i].Slope = a, b
		if math.IsNaN(r.RegressionResults[i].Slope) {
			r.debug_func("RTTRegressAll", fmt.Sprintf("encountered NaN, length of regression input: %d, b: %f",
				len(reg_input.X.Iter()), b))
		}
	}

}

func (r *RTTMonitor) getRateStatus() float64 {
	// longterm_slope := r.RegressionResults[0].Slope
	shortterm_slope := r.RegressionResults[len(r.RegressionResults)-1].Slope

	score := r.slopescorer_short.score(r.RegressionResults[1].Slope)
	r.debug_func("RTTRegress_slopescore_short", fmt.Sprintf("%f", score))
	r.debug_func("RTTRegress_slopescore_combined", fmt.Sprintf("%f", score))
	return score

	r.debug_func("RTTRegress-Shortterm", fmt.Sprintf("%f", r.RegressionResults[2].Slope))
	r.debug_func("RTTRegress-Midterm", fmt.Sprintf("%f", r.RegressionResults[1].Slope))
	r.debug_func("RTTRegress-Longterm", fmt.Sprintf("%f", r.RegressionResults[0].Slope))

	r.debug_func("RTTRegress_slopescore_short", fmt.Sprintf("%f", r.slopescorer_short.score(shortterm_slope)))
	r.debug_func("RTTRegress_slopescore_combined", fmt.Sprintf("%f", r.slopescorer_long.score(r.RegressionResults[1].Slope)))
	return 0.1
}
