package qlog

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"
	"github.com/quic-go/quic-go/internal/wire"
	"github.com/quic-go/quic-go/logging"
)

type Balancer struct {
	last_bidi_frame  time.Time
	connectionTracer *logging.ConnectionTracer
}

func (b *Balancer) UpdateLastBidiFrame() {
	b.last_bidi_frame = time.Now()
}

func (b *Balancer) CanSendUniFrame() bool {
	difference := time.Since(b.last_bidi_frame)
	if difference.Abs() < 1_000_000*20 {
		fmt.Println("cant send uniframe")
		return false
	} else {
		fmt.Println("can send uniframe")
		return true
	}
}

func NewBalancerAndTracer(w io.WriteCloser, p logging.Perspective, odcid protocol.ConnectionID) (*logging.ConnectionTracer, *Balancer) {
	ct := NewBalancedConnectionTracer(w, p, odcid)
	balancer := &Balancer{last_bidi_frame: time.Now(), connectionTracer: ct}

	return ct, balancer
}

// NewConnectionTracer creates a new tracer to record a qlog for a connection.
func NewBalancedConnectionTracer(w io.WriteCloser, p logging.Perspective, odcid protocol.ConnectionID) *logging.ConnectionTracer {
	tr := &trace{
		VantagePoint: vantagePoint{Type: p},
		CommonFields: commonFields{
			ODCID:         odcid,
			GroupID:       odcid,
			ReferenceTime: time.Now(),
		},
	}
	t := connectionTracer{
		w:           *newWriter(w, tr),
		perspective: p,
	}
	go t.w.Run()
	return &logging.ConnectionTracer{
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
			t.recordEvent(time.Now(), eventALPNInformation{chosenALPN: protocol})
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
}
