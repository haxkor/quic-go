package streamtypebalancer

import (
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
)

type streamClassInfo struct {
	rateMonitor *RateMonitor

	debug_func func(name, msg string)

	cc_data struct {
		timeframe     time.Duration
		allowed_bytes protocol.ByteCount
		lastmax       protocol.ByteCount
		growing       growingStage
	}
}

func newStreamClassInfo(ratemonitor *RateMonitor) *streamClassInfo {
	sci := streamClassInfo{rateMonitor: ratemonitor}
	sci.cc_data.timeframe = time.Second
	sci.cc_data.allowed_bytes = 10
	sci.cc_data.lastmax = 1
	sci.cc_data.growing = UNI_INCREASING_SLOWLY
	sci.debug_func = ratemonitor.debug_func

	return &sci
}

func (sci *streamClassInfo) getRateStatus() float64 {
	return sci.rateMonitor.getRateStatus()
}

func (sci *streamClassInfo) getCurrentbitrateToMax() float64 {
	return float64(sci.rateMonitor.GetBitrateWithinMediantimeframe()) / float64(sci.rateMonitor.GetMaxMedian())
}

func (sci *streamClassInfo) multiplyAllowedBytes(factor float64) {
	sci.cc_data.allowed_bytes = max(10, protocol.ByteCount(float64(sci.cc_data.allowed_bytes)*(factor)))
}
