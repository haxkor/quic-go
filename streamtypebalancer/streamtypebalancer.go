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
	"sync"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils"
	"github.com/quic-go/quic-go/internal/utils/ringbuffer"
	"github.com/quic-go/quic-go/internal/wire"
	"github.com/quic-go/quic-go/logging"
	"github.com/quic-go/quic-go/qlog"
)

type growingStage int

const (
	UNI_INCREASING = iota
	UNI_INCREASING_SLOWLY
	UNI_DECREASING
	UNI_DECREASING_GENTLE
)

func growingStageToStr(s growingStage) string {
	switch s {
	case UNI_INCREASING:
		return "UNI_INCREASING"
	case UNI_INCREASING_SLOWLY:
		return "UNI_INCREASING_SLOWLY"
	case UNI_DECREASING:
		return "UNI_DECREASING"
	case UNI_DECREASING_GENTLE:
		return "UNI_DECREASING_GENTLE"
	default:
		return "ERROR"
	}
}

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

	uni_cc_data struct {
		mutex         sync.Mutex
		timeframe     time.Duration
		allowed_bytes protocol.ByteCount
		lastmax       protocol.ByteCount
		growing       growingStage
	}

	bidirateMonitor *RateMonitor
	unirateMonitor  *RateMonitor

	rttMonitor  *RTTMonitor
	oldRTTStats utils.RTTStats
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
	balancer := &Balancer{last_bidi_frame: time.Now()}
	balancer.uni_cc_data.timeframe = time.Millisecond * 500
	balancer.uni_cc_data.allowed_bytes = 100

	balancer.bidirateMonitor = NewRateMonitor([]time.Duration{time.Second * 5, time.Millisecond * 400})
	balancer.bidirateMonitor.debug_func = balancer.Debug

	balancer.unirateMonitor = NewRateMonitor([]time.Duration{time.Second})
	balancer.unirateMonitor.debug_func = balancer.Debug

	balancer.rttMonitor = NewRTTMonitor([]time.Duration{time.Second * 3, time.Second * 1, time.Millisecond * 400})
	balancer.rttMonitor.debug_func = balancer.Debug

	t := qlog.NewConnectionTracer_tracer(w, p, odcid)
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

	go balancer.LogMonitorResultsLoop()
	// go balancer.callRTTMonitor()

	return &connection_tracer, balancer
}

func (b *Balancer) LogMonitorResultsLoop() {
	for {
		time.Sleep(time.Millisecond * 100)
		b.LogMonitorResults()
	}
}

func (b *Balancer) LogMonitorResults() {
	b.bidirateMonitor.RegressAll()
	b.Debug("LogMonitorResults:", b.bidirateMonitor.Summary())

	b.UpdateUnirate()
}

func (b *Balancer) callRTTMonitor() {
	for {
		time.Sleep(time.Millisecond * 100)

		b.rttMonitor.RegressAll()
		b.rttMonitor.getRateStatus()
	}
}

func (b *Balancer) UpdateUnirate() {
	uni_growth := 1.3
	reason := ""

	rateStatus := b.bidirateMonitor.getRateStatus()
	b.Debug("UpdateUnirate-rateStatus", fmt.Sprintf("%f", rateStatus))

	// uni_growth = (3 + rateStatus) / 4
	if rateStatus < 0.9 {
		uni_growth *= (rateStatus * rateStatus)
	} else if rateStatus > 1.0 {
		uni_growth *= min(rateStatus, 1.5)
	}

	bitrate_ratio := float64(b.bidirateMonitor.GetBitrateWithinMediantimeframe()) / float64(b.bidirateMonitor.GetMaxMedian())
	if bitrate_ratio < 0.5 {
		uni_growth = -1
		// b.uni_cc_data.allowed_bytes = 10
		// b.uni_cc_data.lastmax = 10

	} else if bitrate_ratio < 1 {
		// b.uni_cc_data.allowed_bytes = protocol.ByteCount(float64(b.uni_cc_data.allowed_bytes) * 0.80)
		// uni_growth -= 0.2
		uni_growth *= (bitrate_ratio * bitrate_ratio)
		b.Debug("UpdateUnirate", "current bitrate smaller than median of max bitrates, decreasing")
		reason += "bidirate smaller than median, "
	}

	b.rttMonitor.RegressAll()
	rttStatus := b.rttMonitor.getRateStatus()
	if rttStatus > 0.8 {
		b.Debug("UpdateUnirate", "RTT_INCREASING")
		reason += "RTT increasing, "
		uni_growth *= 0.7
	}

	if reason != "" {
		b.Debug("UpdateUnirate", fmt.Sprintf("reason: %s", reason))
	}

	//alles faktoren 0-1
	//multiplizieren
	//dann hat man den growthfactor
	//das muss man jetzt noch irgendwie abflachen über die zeit

	//if we are not using the full potential, dont grow the allowed rate further
	if b.unirateMonitor.getBitrateWithin(b.uni_cc_data.timeframe) <
		protocol.ByteCount(float64(b.uni_cc_data.allowed_bytes)*0.9) {
		uni_growth = min(0.99, uni_growth)
	}

	// new_allowed_bytes := max(10, protocol.ByteCount(float64(b.uni_cc_data.allowed_bytes)*uni_growth))

	// we are at a downward change
	if uni_growth < 0.9 &&
		(b.uni_cc_data.growing == UNI_INCREASING || b.uni_cc_data.growing == UNI_INCREASING_SLOWLY) {
		// if we hit a different limit than last time, update
		ratio := float64(b.uni_cc_data.lastmax) / (float64(b.uni_cc_data.allowed_bytes) * 1)
		b.Debug("hit a limit, start to decrease, ratio: ", fmt.Sprintf("%f", ratio))
		if !(0.7 < ratio && ratio < 1.2) {
			// b.uni_cc_data.lastmax = protocol.ByteCount(float64(b.uni_cc_data.allowed_bytes) * uni_growth)
			b.uni_cc_data.lastmax = b.unirateMonitor.getBitrateWithin(b.uni_cc_data.timeframe)
			b.Debug("updated lastmax:", fmt.Sprintf("%d", b.uni_cc_data.lastmax))
			b.uni_cc_data.growing = UNI_DECREASING
			b.Debug("UpdateUnirate-growing", "set to DECREASING")
		} else { //we hit the same max as last time, lets be gentle in the decline
			// old_allowed_bytes := new_allowed_bytes
			// new_allowed_bytes = max(10, protocol.ByteCount(float64(b.uni_cc_data.allowed_bytes)*0.8)) //*((1+uni_growth)/2)))
			b.uni_cc_data.growing = UNI_DECREASING_GENTLE
			b.Debug("UpdateUnirate-growing", "set to GENTLE")

			// b.Debug("gentle decline", fmt.Sprintf("old %d, gentle %d", old_allowed_bytes, new_allowed_bytes))
		}

	} else if uni_growth > 1 {

		if b.uni_cc_data.allowed_bytes > b.uni_cc_data.lastmax {
			b.uni_cc_data.lastmax = b.uni_cc_data.allowed_bytes
			b.uni_cc_data.growing = UNI_INCREASING
			b.Debug("updated lastmax:", fmt.Sprintf("%d", b.uni_cc_data.lastmax))
		} else if protocol.ByteCount(float64(b.uni_cc_data.allowed_bytes)*2) > b.uni_cc_data.lastmax {
			// update lastmax?
			b.uni_cc_data.growing = UNI_INCREASING_SLOWLY
			b.Debug("UpdateUnirate-growing", "set to INCREASING_SLOWLY")
		} else {
			b.uni_cc_data.growing = UNI_INCREASING
			b.Debug("UpdateUnirate-growing", "set to INCREASING")
		}

	} else if uni_growth < 1 {
		if uni_growth > 0.5 && b.uni_cc_data.growing == UNI_DECREASING_GENTLE {
			// nothing to change
			b.Debug("UpdateUnirate-growing", "staying at GENTLE")
		} else if uni_growth < 0 {
			b.uni_cc_data.growing = UNI_DECREASING //this changes everything
			b.Debug("UpdateUnirate-growing", "set to DECREASING")
		}

	}

	b.Debug("updated lastmax:", fmt.Sprintf("%d", b.uni_cc_data.lastmax))

	switch b.uni_cc_data.growing {
	case UNI_INCREASING_SLOWLY:
		// b.uni_cc_data.allowed_bytes += 200
		b.uni_cc_data.allowed_bytes = protocol.ByteCount(float64(b.uni_cc_data.allowed_bytes) * (uni_growth + 29) / 30)
	case UNI_INCREASING:
		fallthrough
	case UNI_DECREASING:
		b.uni_cc_data.allowed_bytes = max(10, protocol.ByteCount(float64(b.uni_cc_data.allowed_bytes)*uni_growth))

	case UNI_DECREASING_GENTLE:
		non_gentle := max(10, protocol.ByteCount(float64(b.uni_cc_data.allowed_bytes)*uni_growth))
		b.uni_cc_data.allowed_bytes = max(10, protocol.ByteCount(float64(b.uni_cc_data.allowed_bytes)*0.95)) //*((1+uni_growth)/2)))
		b.Debug("gentle decline", fmt.Sprintf("old %d, gentle %d", non_gentle, b.uni_cc_data.allowed_bytes))
	}

	b.Debug("UpdateUnirate_allowed_bytes", fmt.Sprintf("%d", b.uni_cc_data.allowed_bytes))
	b.Debug("UpdateUnirate_growth", fmt.Sprintf("%f", uni_growth))
	b.Debug("UpdateUnirate_stage:", growingStageToStr(b.uni_cc_data.growing))

}

func (b *Balancer) UpdateMetrics(rttStats *logging.RTTStats, cwnd, bytesInFlight protocol.ByteCount, packetsInFlight int) {
	if rttStats.LatestRTT() != b.oldRTTStats.LatestRTT() {
		b.oldRTTStats = *rttStats // this is not very efficient but we save all information
		b.rttMonitor.AddSample(rttStats.LatestRTT())
	}
}

func (b *Balancer) UpdatedCongestionState(state logging.CongestionState) {
	// fmt.Printf("streamtypebalancer updatecongestionstate: %s\n", state)
}

func (b *Balancer) Debug(name, msg string) {
	b.connectionTracer.Debug(name, msg)
}

func (b *Balancer) CanSendUniFrame(size protocol.ByteCount) bool {
	if b.unirateMonitor.getBitrateWithin(b.uni_cc_data.timeframe) > b.uni_cc_data.allowed_bytes {
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

func (b *Balancer) RegisterSentBytes(size protocol.ByteCount, streamtype protocol.StreamType) {
	if streamtype == protocol.StreamTypeBidi {
		// b.monitorBidiRate()

		b.bidirateMonitor.AddSentData(size)

	} else if streamtype == protocol.StreamTypeUni {
		b.unibytesSentList.PushBack(SentTuple{time.Now(), size})
		b.unirateMonitor.AddSentData(size)
	}
}
