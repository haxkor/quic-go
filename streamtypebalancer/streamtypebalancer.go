package streamtypebalancer

import (
	"fmt"
	"io"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/logging"
	"github.com/quic-go/quic-go/qlog"
)

type Balancer struct {
	last_bidi_frame  time.Time
	connectionTracer *logging.ConnectionTracer
}

func MyNewTracer(w io.WriteCloser, p logging.Perspective, odcid protocol.ConnectionID) *logging.ConnectionTracer {
	// b := Balancer{last_bidi_frame: time.Now()}
	tracer := qlog.NewConnectionTracer_tracer(w, p, odcid)

	connection_tracer := logging.ConnectionTracer{
		UpdatedMetrics: func(rttStats *logging.RTTStats, cwnd, bytesInFlight logging.ByteCount, packetsInFlight int) {
			tracer.UpdatedMetrics(rttStats, cwnd, bytesInFlight, packetsInFlight)
		},
		UpdatedCongestionState: func(state logging.CongestionState) {
			tracer.UpdatedCongestionState(state)
		},
	}

	return &connection_tracer
}

func NewBalancerAndTracer(w io.WriteCloser, p logging.Perspective, odcid protocol.ConnectionID) (*logging.ConnectionTracer, *Balancer) {
	tracer := MyNewTracer(w, p, odcid)
	balancer := &Balancer{last_bidi_frame: time.Now(), connectionTracer: tracer}

	return tracer, balancer
}

func (b *Balancer) UpdateMetrics(rttStats *logging.RTTStats, cwnd, bytesInFlight protocol.ByteCount, packetsInFlight int) {
	fmt.Println("streamtypebalancer update metrics")
	return
}

func (b *Balancer) UpdatedCongestionState(state logging.CongestionState) {
	fmt.Printf("streamtypebalancer updatecongestionstate: %s\n", state)
}
