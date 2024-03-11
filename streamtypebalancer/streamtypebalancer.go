package streamtypebalancer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"
	"github.com/quic-go/quic-go/internal/utils/ringbuffer"
	"github.com/quic-go/quic-go/internal/wire"
	"github.com/quic-go/quic-go/logging"
	"github.com/quic-go/quic-go/qlog"
)

type SentTuple struct {
	timestamp  time.Time
	bytes_sent protocol.ByteCount
}

type Balancer struct {
	last_bidi_frame  time.Time
	connectionTracer *logging.ConnectionTracer
	cwnd             protocol.ByteCount
	bytesInFlight    protocol.ByteCount

	unibytesSentList ringbuffer.RingBuffer[SentTuple]
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
		log.Printf("qlogdir not set")
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

func NewBalancerAndTracer(w io.WriteCloser, p logging.Perspective, odcid protocol.ConnectionID) (*logging.ConnectionTracer, *Balancer) {
	t := qlog.NewConnectionTracer_tracer(w, p, odcid)
	balancer := &Balancer{last_bidi_frame: time.Now()}

	connection_tracer := logging.ConnectionTracer{
		StartedConnection: func(local, remote net.Addr, srcConnID, destConnID logging.ConnectionID) {
			t.StartedConnection(local, remote, srcConnID, destConnID)
		},
		NegotiatedVersion: func(chosen logging.VersionNumber, clientVersions, serverVersions []logging.VersionNumber) {
			t.NegotiatedVersion(chosen, clientVersions, serverVersions)
		},
		ClosedConnection:            func(e error) { t.ClosedConnection(e) },
		SentTransportParameters:     func(tp *wire.TransportParameters) { t.SentTransportParameters(tp) },
		ReceivedTransportParameters: func(tp *wire.TransportParameters) { t.ReceivedTransportParameters(tp) },
		RestoredTransportParameters: func(tp *wire.TransportParameters) { t.RestoredTransportParameters(tp) },
		SentLongHeaderPacket: func(hdr *logging.ExtendedHeader, size logging.ByteCount, ecn logging.ECN, ack *logging.AckFrame, frames []logging.Frame) {
			t.SentLongHeaderPacket(hdr, size, ecn, ack, frames)
		},
		SentShortHeaderPacket: func(hdr *logging.ShortHeader, size logging.ByteCount, ecn logging.ECN, ack *logging.AckFrame, frames []logging.Frame) {
			t.SentShortHeaderPacket(hdr, size, ecn, ack, frames)
		},
		ReceivedLongHeaderPacket: func(hdr *logging.ExtendedHeader, size logging.ByteCount, ecn logging.ECN, frames []logging.Frame) {
			t.ReceivedLongHeaderPacket(hdr, size, ecn, frames)
		},
		ReceivedShortHeaderPacket: func(hdr *logging.ShortHeader, size logging.ByteCount, ecn logging.ECN, frames []logging.Frame) {
			t.ReceivedShortHeaderPacket(hdr, size, ecn, frames)
		},
		ReceivedRetry: func(hdr *wire.Header) {
			t.ReceivedRetry(hdr)
		},
		ReceivedVersionNegotiationPacket: func(dest, src logging.ArbitraryLenConnectionID, versions []logging.VersionNumber) {
			t.ReceivedVersionNegotiationPacket(dest, src, versions)
		},
		BufferedPacket: func(pt logging.PacketType, size protocol.ByteCount) {
			t.BufferedPacket(pt, size)
		},
		DroppedPacket: func(pt logging.PacketType, pn logging.PacketNumber, size logging.ByteCount, reason logging.PacketDropReason) {
			t.DroppedPacket(pt, pn, size, reason)
		},
		UpdatedMetrics: func(rttStats *utils.RTTStats, cwnd, bytesInFlight protocol.ByteCount, packetsInFlight int) {
			balancer.UpdateMetrics(rttStats, cwnd, bytesInFlight, packetsInFlight)
			t.UpdatedMetrics(rttStats, cwnd, bytesInFlight, packetsInFlight)
		},
		LostPacket: func(encLevel protocol.EncryptionLevel, pn protocol.PacketNumber, lossReason logging.PacketLossReason) {
			t.LostPacket(encLevel, pn, lossReason)
		},
		UpdatedCongestionState: func(state logging.CongestionState) {
			t.UpdatedCongestionState(state)
		},
		UpdatedPTOCount: func(value uint32) {
			t.UpdatedPTOCount(value)
		},
		UpdatedKeyFromTLS: func(encLevel protocol.EncryptionLevel, pers protocol.Perspective) {
			t.UpdatedKeyFromTLS(encLevel, pers)
		},
		UpdatedKey: func(generation protocol.KeyPhase, remote bool) {
			t.UpdatedKey(generation, remote)
		},
		DroppedEncryptionLevel: func(encLevel protocol.EncryptionLevel) {
			t.DroppedEncryptionLevel(encLevel)
		},
		DroppedKey: func(generation protocol.KeyPhase) {
			t.DroppedKey(generation)
		},
		SetLossTimer: func(tt logging.TimerType, encLevel protocol.EncryptionLevel, timeout time.Time) {
			t.SetLossTimer(tt, encLevel, timeout)
		},
		LossTimerExpired: func(tt logging.TimerType, encLevel protocol.EncryptionLevel) {
			t.LossTimerExpired(tt, encLevel)
		},
		LossTimerCanceled: func() {
			t.LossTimerCanceled()
		},
		ECNStateUpdated: func(state logging.ECNState, trigger logging.ECNStateTrigger) {
			t.ECNStateUpdated(state, trigger)
		},
		ChoseALPN: func(protocol string) {
			//t.recordEvent(time.Now(), eventALPNInformation{chosenALPN: protocol})
		},
		Debug: func(name, msg string) {
			t.Debug(name, msg)
		},
		Close: func() {
			t.Close()
		},
		FrameReadFromRingbuffer: func() {
			t.FrameReadFromRingbuffer()
		},
		NewFrameToRingbuffer: func(unidirectional bool) {
			t.NewFrameToRingbuffer(unidirectional)
		},
	}

	balancer.connectionTracer = &connection_tracer

	return &connection_tracer, balancer
}

func (b *Balancer) UpdateMetrics(rttStats *logging.RTTStats, cwnd, bytesInFlight protocol.ByteCount, packetsInFlight int) {
	if cwnd != b.cwnd {
		msg := fmt.Sprintf("cwnd changed from %d to %d\tbytesInFlight:%d", b.cwnd, cwnd, bytesInFlight)
		b.connectionTracer.Debug("UpdateMetrics", msg)
	}
	b.cwnd = cwnd
	b.bytesInFlight = bytesInFlight
}

func (b *Balancer) UpdatedCongestionState(state logging.CongestionState) {
	// fmt.Printf("streamtypebalancer updatecongestionstate: %s\n", state)
}

func (b *Balancer) Debug(name, msg string) {
	b.connectionTracer.Debug(name, msg)
}

func (b *Balancer) UpdateLastBidiFrame() {
	diff := time.Since(b.last_bidi_frame)
	b.last_bidi_frame = time.Now()
	b.Debug("UpdateLastBidiFrame", fmt.Sprintf("NewBidiFrame, difference: %s", diff))
}

func (b *Balancer) CanSendUniFrame(size protocol.ByteCount) bool {
	// b.reportOnStatus()

	TIMEFRAME := time.Millisecond * 50
	if b.sumOfSentBytes(TIMEFRAME) > 3000 {

		// if b.bytesInFlight > b.cwnd/2 {
		// if time.Since(b.last_bidi_frame).Abs() < 1_000_000*10 {
		b.Debug("CanSendUniFrame:", "cant send uniframe")
		return false
	} else {
		b.Debug("CanSendUniFrame:", "can send uniframe")
		return true
	}
}

func (b *Balancer) reportOnStatus() {
	difference := time.Since(b.last_bidi_frame)
	msg := fmt.Sprintf("time since last bidiframe: %s\ncwnd: %d  bytesInFlight: %d",
		difference.String(), b.cwnd, b.bytesInFlight)
	b.connectionTracer.Debug("balancer status report:", msg)
}

func (b *Balancer) sumOfSentBytes(within_timeframe time.Duration) protocol.ByteCount {
	now := time.Now()

	// remove old sizes
	for !b.unibytesSentList.Empty() {
		timepassed := now.Sub(b.unibytesSentList.PeekFront().timestamp)
		b.Debug("sendUniFrameSize", fmt.Sprintf("time since sending: %s", timepassed.String()))
		if timepassed > within_timeframe {
			b.unibytesSentList.PopFront()
		} else {
			break
		}
	}

	var bytes_sum protocol.ByteCount = 0
	for _, next_elem := range b.unibytesSentList.Iter() {
		bytes_sum += next_elem.bytes_sent
	}
	b.Debug("sendUniFrameSize", fmt.Sprintf("sum = %d", bytes_sum))
	return bytes_sum
}

func (b *Balancer) RegisterSentBytes(size protocol.ByteCount, streamtype protocol.StreamType) {
	if streamtype == protocol.StreamTypeBidi {
		return
	} else if streamtype == protocol.StreamTypeUni {
		b.unibytesSentList.PushBack(SentTuple{time.Now(), size})
	}
}
