package streamtypebalancer

import (
	"fmt"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/logging"
)

type Balancer struct {
	last_bidi_frame  time.Time
	connectionTracer *logging.ConnectionTracer
}

func MyNewTracer() *logging.ConnectionTracer {
	b := Balancer{last_bidi_frame: time.Now()}

	tracer := logging.ConnectionTracer{
		UpdatedMetrics: func(rttStats *logging.RTTStats, cwnd, bytesInFlight logging.ByteCount, packetsInFlight int) {
			b.UpdateMetrics(rttStats, cwnd, bytesInFlight, packetsInFlight)
		},
		UpdatedCongestionState: func(state logging.CongestionState) {
			b.UpdatedCongestionState(state)
		},
	}

	return &tracer
}

type balancer struct {
	number int
}

func (b *Balancer) UpdateMetrics(rttStats *logging.RTTStats, cwnd, bytesInFlight protocol.ByteCount, packetsInFlight int) {
	fmt.Println("streamtypebalancer update metrics")
	return
}

func (b *Balancer) UpdatedCongestionState(state logging.CongestionState) {
	fmt.Printf("streamtypebalancer updatecongestionstate: %s\n", state)
}
