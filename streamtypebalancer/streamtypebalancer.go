package streamtypebalancer

import (
	"fmt"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/logging"
)

func MyNewTracer() *logging.ConnectionTracer {
	b := balancer{
		number: 0,
	}

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

func (b *balancer) UpdateMetrics(rttStats *logging.RTTStats, cwnd, bytesInFlight protocol.ByteCount, packetsInFlight int) {
	fmt.Println("streamtypebalancer update metrics")
	return
}

func (b *balancer) UpdatedCongestionState(state logging.CongestionState) {
	fmt.Printf("streamtypebalancer updatecongestionstate: %s\n", state)
}
