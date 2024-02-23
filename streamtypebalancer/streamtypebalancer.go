package streamtypebalancer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"
	"github.com/quic-go/quic-go/logging"
	"github.com/quic-go/quic-go/qlog"
)

type Balancer struct {
	last_bidi_frame  time.Time
	connectionTracer *logging.ConnectionTracer
}

func FunctionForBalancerAndTracer(_ context.Context, p protocol.Perspective, connID protocol.ConnectionID) (*logging.ConnectionTracer, *Balancer) {
	var label string
	switch p {
	case logging.PerspectiveClient:
		label = "client"
	case logging.PerspectiveServer:
		label = "server"
	}
	qlogDir := os.Getenv("QLOGDIR")
	if qlogDir == "" {
		return nil, nil
	}
	if _, err := os.Stat(qlogDir); os.IsNotExist(err) {
		if err := os.MkdirAll(qlogDir, 0o755); err != nil {
			log.Fatalf("failed to create qlog dir %s: %v", qlogDir, err)
		}
	}
	timestamp := time.Now().Format("2006-01-02-15:04:05")
	path := fmt.Sprintf("%s/%s%s.qlog", strings.TrimRight(qlogDir, "/"), timestamp, label)
	f, err := os.Create(path)
	if err != nil {
		log.Printf("Failed to create qlog file %s: %s", path, err.Error())
		return nil, nil
	}
	return NewBalancerAndTracer(utils.NewBufferedWriteCloser(bufio.NewWriter(f), f), p, connID)

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
