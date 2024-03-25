package streamtypebalancer

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils/ringbuffer"

	"gonum.org/v1/gonum/stat"
)

type SentByteTuple struct {
	ts   time.Time
	sent protocol.ByteCount
}

func (t *SentByteTuple) toFloats() (x, y float64) {
	return float64(t.ts.UnixMilli()), float64(t.sent)
}

type regressionResult struct {
	Offset float64
	Slope  float64
}

type RateMonitor struct {
	sentqueue         ringbuffer.RingBuffer[SentByteTuple]
	sentqueue_mutex   sync.Mutex
	timeframes        []time.Duration
	RegressionResults []regressionResult
	total_bytes_send  protocol.ByteCount

	debug_func func(name, msg string)

	sentDataInput chan protocol.ByteCount

	medianControl struct {
		holder      *bitrateHolder
		pollEvery   time.Duration
		bitrateOver time.Duration
	}
}

func NewRateMonitor(timeframes []time.Duration) *RateMonitor {
	r := RateMonitor{timeframes: timeframes}
	r.sentqueue.Init(32)
	r.RegressionResults = make([]regressionResult, len(timeframes))
	r.sentDataInput = make(chan protocol.ByteCount)

	r.medianControl.holder = NewBitrateHolder(20)
	r.medianControl.bitrateOver = time.Second * 2
	r.medianControl.pollEvery = time.Millisecond * 500

	go r.loopaddSentDataActual()
	go r.recordBitrates()

	return &r
}

func (r *RateMonitor) AddSentData(size protocol.ByteCount) {
	r.sentDataInput <- size
}

func (r *RateMonitor) loopaddSentDataActual() {
	for {
		r.addSentDataActual()
	}
}

func (r *RateMonitor) GetBitrateWithinMediantimeframe() protocol.ByteCount {
	return r.getBitrateWithin(r.medianControl.bitrateOver)
}

func (r *RateMonitor) getBitrateWithin(tf time.Duration) protocol.ByteCount {
	r.sentqueue_mutex.Lock()
	bitrates := r.sentqueue.Iter()
	r.sentqueue_mutex.Unlock()

	if len(bitrates) == 0 {
		return 0
	}
	now := time.Now()
	r.PopOld(tf*2, now)
	latest := bitrates[len(bitrates)-1]

	for _, senttuple := range bitrates {
		if now.Sub(senttuple.ts) <= tf {
			r.debug_func("getBitrateWithin", fmt.Sprintf("returning latest: %d, senttuple: %d", latest.sent, senttuple.sent))
			return latest.sent - senttuple.sent
		}
	}
	panic("getBitrateWithin: unreachable")
}

func (r *RateMonitor) recordBitrates() {
	holder := r.medianControl.holder
	for {
		time.Sleep(r.medianControl.pollEvery)
		bitrate_within := r.getBitrateWithin(r.medianControl.bitrateOver)
		holder.Add(bitrate_within)
		holder.shrink(0.95)
		r.debug_func("bitrateHolder", fmt.Sprintf("after shrink median: %d, bitrate: %d",
			holder.getMedian(), bitrate_within))
	}
}

func (r *RateMonitor) GetMaxMedian() protocol.ByteCount {
	result := r.medianControl.holder.getMedian()
	r.debug_func("GetMaxMedian", fmt.Sprintf("median: %d", result))
	return result
}

func (r *RateMonitor) addSentDataActual() {
	size := <-r.sentDataInput
	r.sentqueue_mutex.Lock()
	defer r.sentqueue_mutex.Unlock()
	r.total_bytes_send += size
	r.sentqueue.PushBack(SentByteTuple{time.Now(), r.total_bytes_send})
	r.debug_func("sentdata receiver", fmt.Sprintf("total byte sent: %d", r.total_bytes_send))
}

type regressionInput struct {
	X ringbuffer.RingBuffer[float64]
	Y ringbuffer.RingBuffer[float64]
}

func (r *regressionInput) init() {
	r.X.Init(8)
	r.Y.Init(8)
}

func (r *RateMonitor) PopOld(more_than time.Duration, since time.Time) {
	for !r.sentqueue.Empty() &&
		since.Sub(r.sentqueue.PeekFront().ts) > more_than {
		r.sentqueue.PopFront()
	}
}

func (r *RateMonitor) RegressAll() {
	r.sentqueue_mutex.Lock()
	now := time.Now()

	//the loop below assumes the initially met samples are within the first (biggest) timeframe
	//therefore, we need to first remove all samples that dont meet this requirement
	r.PopOld(r.timeframes[0], now)

	samples := r.sentqueue.Iter()
	r.sentqueue_mutex.Unlock()

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
			r.debug_func("RegressAll", fmt.Sprintf("encountered NaN, length of regression input: %d, b: %f",
				len(reg_input.X.Iter()), b))
		}
	}

}

func (r *RateMonitor) Summary() string {
	summary := ""

	for i, reg_result := range r.RegressionResults {
		summary += fmt.Sprintf("for timeframe %s the slope is %f\n",
			r.timeframes[i], reg_result.Slope)
	}

	return summary
}

type rateStatus int

const (
	STEADY rateStatus = iota
	INCREASING
	DECREASING
)

func (r *RateMonitor) getRateStatus() rateStatus {
	longterm_slope := r.RegressionResults[0].Slope
	shortterm_slope := r.RegressionResults[len(r.RegressionResults)-1].Slope

	ratio := float64(longterm_slope) / float64(shortterm_slope)

	if ratio > 1.10 { // longterm rate was growing faster
		return DECREASING
	} else if ratio < 0.9 { // shortterm is growing more
		return INCREASING
	} else {
		return STEADY
	}
}
