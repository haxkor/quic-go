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

type Balancer struct {
	connectionTracer *logging.ConnectionTracer

	streamclasses []streamClassInfo
	reststreams   streamClassInfo
	bidi_info     streamClassInfo

	rttMonitor  *RTTMonitor
	oldRTTStats utils.RTTStats

	stream_to_index map[protocol.StreamID]int
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

func (b *Balancer) addPriorityStream(id protocol.StreamID) {
	b.stream_to_index[id] = 1

}

func NewBalancerAndTracer(w io.WriteCloser, p logging.Perspective, odcid protocol.ConnectionID) (*logging.ConnectionTracer, *Balancer) {
	balancer := &Balancer{}

	balancer.stream_to_index = make(map[protocol.StreamID]int)

	// initialize both infos
	rest_monitor := NewRateMonitor([]time.Duration{time.Second})
	rest_monitor.debug_func = balancer.Debug
	rest_info := streamClassInfo{rateMonitor: rest_monitor}
	rest_info.cc_data.timeframe = time.Second
	rest_info.cc_data.allowed_bytes = 10
	rest_info.cc_data.lastmax = 1
	rest_info.cc_data.growing = UNI_INCREASING_SLOWLY
	balancer.reststreams = rest_info

	bidirateMonitor := NewRateMonitor([]time.Duration{time.Second * 5, time.Millisecond * 400})
	bidirateMonitor.debug_func = balancer.Debug

	bidi_info := streamClassInfo{rateMonitor: bidirateMonitor}
	bidi_info.cc_data.timeframe = time.Second
	bidi_info.cc_data.allowed_bytes = 10
	bidi_info.cc_data.lastmax = 1
	bidi_info.cc_data.growing = UNI_INCREASING_SLOWLY

	balancer.bidi_info = bidi_info

	// add to list
	balancer.streamclasses = []streamClassInfo{rest_info, bidi_info}

	//monitor
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

	return &connection_tracer, balancer
}

func (b *Balancer) LogMonitorResultsLoop() {
	for {
		time.Sleep(time.Millisecond * 100)
		b.UpdateUnirate()
	}
}

func (b *Balancer) UpdateUnirate() {
	uni_growth := 1.2
	reason := ""

	b.bidi_info.rateMonitor.RegressAll()
	rateStatus := b.bidi_info.getRateStatus()
	b.Debug("UpdateUnirate-rateStatus", fmt.Sprintf("%f", rateStatus))

	if rateStatus < 0.9 {
		uni_growth *= (rateStatus * rateStatus)
	} else if rateStatus > 1.0 {
		uni_growth *= min(rateStatus, 1.5)
	}

	bitrate_ratio := b.bidi_info.getCurrentbitrateToMax()
	if bitrate_ratio < 0.5 {
		uni_growth = -1
	} else if bitrate_ratio < 1 {
		uni_growth *= (bitrate_ratio * bitrate_ratio)
		b.Debug("UpdateUnirate", "current bitrate smaller than median of max bitrates, decreasing")
		reason += "bidirate smaller than median, "
	}

	b.rttMonitor.RegressAll() //should this be seperate thread?
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
	if b.reststreams.rateMonitor.getBitrateWithin(b.reststreams.cc_data.timeframe) <
		protocol.ByteCount(float64(b.reststreams.cc_data.allowed_bytes)*0.9) {
		uni_growth = min(0.99, uni_growth)
	}
	uni_growth = min(1.5, uni_growth)

	// we are at a downward change
	if uni_growth < 0.9 &&
		(b.reststreams.cc_data.growing == UNI_INCREASING || b.reststreams.cc_data.growing == UNI_INCREASING_SLOWLY) {
		ratio := float64(b.reststreams.cc_data.lastmax) / (float64(b.reststreams.cc_data.allowed_bytes) * 1)
		b.Debug("hit a limit, start to decrease, ratio: ", fmt.Sprintf("%f", ratio))
		// if we hit a different limit than last time, update
		if !(0.7 < ratio && ratio < 1.2) {
			b.reststreams.cc_data.lastmax = b.reststreams.rateMonitor.getBitrateWithin(b.reststreams.cc_data.timeframe)
			b.Debug("updated lastmax:", fmt.Sprintf("%d", b.reststreams.cc_data.lastmax))
			b.reststreams.cc_data.growing = UNI_DECREASING
			b.Debug("UpdateUnirate-growing", "set to DECREASING")
		} else { //we hit the same max as last time, lets be gentle in the decline
			b.reststreams.cc_data.growing = UNI_DECREASING_GENTLE
			b.Debug("UpdateUnirate-growing", "set to GENTLE")
		}
	} else if uni_growth > 1 {

		//if we exceed the lastmax, apparently there is no problem, we can grow normal
		if b.reststreams.cc_data.allowed_bytes > b.reststreams.cc_data.lastmax {
			b.reststreams.cc_data.lastmax = b.reststreams.cc_data.allowed_bytes
			b.reststreams.cc_data.growing = UNI_INCREASING

			//if we are approaching the lastmax, grow carefully
		} else if protocol.ByteCount(float64(b.reststreams.cc_data.allowed_bytes)*2) > b.reststreams.cc_data.lastmax {
			b.reststreams.cc_data.growing = UNI_INCREASING_SLOWLY
			b.Debug("UpdateUnirate-growing", "set to INCREASING_SLOWLY")
		} else {
			b.reststreams.cc_data.growing = UNI_INCREASING
			b.Debug("UpdateUnirate-growing", "set to INCREASING")
		}
	} else if uni_growth < 1 {
		if uni_growth > 0.5 && b.reststreams.cc_data.growing == UNI_DECREASING_GENTLE {
			b.Debug("UpdateUnirate-growing", "staying at GENTLE")
		} else if uni_growth < 0 {
			b.reststreams.cc_data.growing = UNI_DECREASING
			b.Debug("UpdateUnirate-growing", "set to DECREASING")
		}
	}

	switch b.reststreams.cc_data.growing {
	case UNI_INCREASING_SLOWLY:
		b.reststreams.multiplyAllowedBytes((uni_growth + 10) / 11)
	case UNI_DECREASING_GENTLE:
		b.reststreams.multiplyAllowedBytes(0.95)
	default:
		b.reststreams.multiplyAllowedBytes((uni_growth) / 1)
	}

	b.Debug("updated lastmax:", fmt.Sprintf("%d", b.reststreams.cc_data.lastmax))
	b.Debug("UpdateUnirate_allowed_bytes", fmt.Sprintf("%d", b.reststreams.cc_data.allowed_bytes))
	b.Debug("UpdateUnirate_growth", fmt.Sprintf("%f", uni_growth))
	b.Debug("UpdateUnirate_stage:", growingStageToStr(b.reststreams.cc_data.growing))
}

func (b *Balancer) UpdateMetrics(rttStats *logging.RTTStats, cwnd, bytesInFlight protocol.ByteCount, packetsInFlight int) {
	if rttStats.LatestRTT() != b.oldRTTStats.LatestRTT() {
		b.oldRTTStats = *rttStats // this is not very efficient but we save all information
		b.rttMonitor.AddSample(rttStats.LatestRTT())
	}
}

func (b *Balancer) Debug(name, msg string) {
	b.connectionTracer.Debug(name, msg)
}

func (b *Balancer) CanSendUniFrame(size protocol.ByteCount) bool {
	if b.reststreams.rateMonitor.getBitrateWithin(b.reststreams.cc_data.timeframe) > b.reststreams.cc_data.allowed_bytes {
		b.Debug("CanSendUniFrame:", "cant send uniframe")
		return false
	} else {
		b.Debug("CanSendUniFrame:", "can send uniframe")
		return true
	}
}

func (b *Balancer) RegisterSentBytes(size protocol.ByteCount, streamid protocol.StreamID) {
	which_info := b.stream_to_index[streamid]
	// if streamid.Type() == protocol.StreamTypeBidi {
	// 	which_info = 1
	// }
	info := b.streamclasses[which_info]
	b.Debug("RegisterSentBytes:", fmt.Sprintf("which_info: %d", which_info))

	info.addSentData(size)
}

func (b *Balancer) Prioritize(streamid protocol.StreamID) {
	b.stream_to_index[streamid] = 1
}

func (b *Balancer) IsPriority(streamid protocol.StreamID) bool {
	_, found := b.stream_to_index[streamid]
	return found
}
