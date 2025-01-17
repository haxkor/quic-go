package quic

import (
	"errors"
	"fmt"
	"sync"

	"github.com/quic-go/quic-go/internal/ackhandler"
	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/utils/ringbuffer"
	"github.com/quic-go/quic-go/internal/wire"
	"github.com/quic-go/quic-go/logging"
	"github.com/quic-go/quic-go/quicvarint"
	"github.com/quic-go/quic-go/streamtypebalancer"
)

type framer interface {
	HasData() bool

	QueueControlFrame(wire.Frame)
	AppendControlFrames([]ackhandler.Frame, protocol.ByteCount, protocol.Version) ([]ackhandler.Frame, protocol.ByteCount)

	AddActiveStream(protocol.StreamID)
	AppendStreamFrames([]ackhandler.StreamFrame, protocol.ByteCount, protocol.Version) ([]ackhandler.StreamFrame, protocol.ByteCount)

	Handle0RTTRejection() error
}

const maxPathResponses = 256

type framerI struct {
	mutex sync.Mutex

	streamGetter streamGetter

	activeStreams map[protocol.StreamID]struct{}
	streamQueue   ringbuffer.RingBuffer[protocol.StreamID]

	controlFrameMutex sync.Mutex
	controlFrames     []wire.Frame
	pathResponses     []*wire.PathResponseFrame

	balancer *streamtypebalancer.Balancer
}

var _ framer = &framerI{}

func newFramer(streamGetter streamGetter, tracer *logging.ConnectionTracer, balancer *streamtypebalancer.Balancer) framer {
	return &framerI{
		streamGetter:  streamGetter,
		activeStreams: make(map[protocol.StreamID]struct{}),
		streamQueue:   ringbuffer.RingBuffer[protocol.StreamID]{Tracer: tracer, Unidirectional: true},
		balancer:      balancer,
	}
}

func (f *framerI) HasData() bool {
	f.mutex.Lock()
	hasData := !f.streamQueue.Empty()
	f.mutex.Unlock()
	if hasData {
		return true
	}
	f.controlFrameMutex.Lock()
	defer f.controlFrameMutex.Unlock()
	return len(f.controlFrames) > 0 || len(f.pathResponses) > 0
}

func (f *framerI) QueueControlFrame(frame wire.Frame) {
	f.controlFrameMutex.Lock()
	defer f.controlFrameMutex.Unlock()

	if pr, ok := frame.(*wire.PathResponseFrame); ok {
		// Only queue up to maxPathResponses PATH_RESPONSE frames.
		// This limit should be high enough to never be hit in practice,
		// unless the peer is doing something malicious.
		if len(f.pathResponses) >= maxPathResponses {
			return
		}
		f.pathResponses = append(f.pathResponses, pr)
		return
	}
	f.controlFrames = append(f.controlFrames, frame)
}

func (f *framerI) AppendControlFrames(frames []ackhandler.Frame, maxLen protocol.ByteCount, v protocol.Version) ([]ackhandler.Frame, protocol.ByteCount) {
	f.controlFrameMutex.Lock()
	defer f.controlFrameMutex.Unlock()

	var length protocol.ByteCount
	// add a PATH_RESPONSE first, but only pack a single PATH_RESPONSE per packet
	if len(f.pathResponses) > 0 {
		frame := f.pathResponses[0]
		frameLen := frame.Length(v)
		if frameLen <= maxLen {
			frames = append(frames, ackhandler.Frame{Frame: frame})
			length += frameLen
			f.pathResponses = f.pathResponses[1:]
		}
	}

	for len(f.controlFrames) > 0 {
		frame := f.controlFrames[len(f.controlFrames)-1]
		frameLen := frame.Length(v)
		if length+frameLen > maxLen {
			break
		}
		frames = append(frames, ackhandler.Frame{Frame: frame})
		length += frameLen
		f.controlFrames = f.controlFrames[:len(f.controlFrames)-1]
	}
	return frames, length
}

func (f *framerI) AddActiveStream(id protocol.StreamID) {
	f.mutex.Lock()
	if _, ok := f.activeStreams[id]; !ok {
		f.streamQueue.PushBack(id)
		f.activeStreams[id] = struct{}{}
	}
	f.mutex.Unlock()
}

func (f *framerI) getStreamQueueLen() int {
	return f.streamQueue.Len()
}

func (f *framerI) debug(name, msg string) {
	if f.balancer != nil {
		f.balancer.Debug(name, msg)
	}
}

func (f *framerI) StreamQueuePop() protocol.StreamID {
	if f.streamQueue.Empty() {
		panic("StreamQueuePop called but queue is empty!")
	}

	len := f.streamQueue.Len()
	var id protocol.StreamID

	f.debug("StreamQueuePop", "new loop")
	for i := 0; i < len; i++ {
		f.debug("StreamQueuePop", "looping")
		id = f.streamQueue.PopFront()
		if f.balancer != nil && f.balancer.IsPriority(id) {
			return id
		}
		f.streamQueue.PushBack(id)
	}
	id = f.streamQueue.PopFront()
	return id
}

func (f *framerI) StreamQueuePushBack(id protocol.StreamID) {
	f.streamQueue.PushBack(id)
}

func (f *framerI) AppendStreamFrames(frames []ackhandler.StreamFrame, maxLen protocol.ByteCount, v protocol.Version) ([]ackhandler.StreamFrame, protocol.ByteCount) {
	startLen := len(frames)
	var length protocol.ByteCount
	f.mutex.Lock()
	// pop STREAM frames, until less than MinStreamFrameSize bytes are left in the packet
	numActiveStreams := f.getStreamQueueLen()
	for i := 0; i < numActiveStreams; i++ {
		remainingLen := maxLen - length

		if protocol.MinStreamFrameSize+length > maxLen {
			break
		}
		id := f.StreamQueuePop()

		if f.balancer != nil && !f.balancer.IsPriority(id) {
			if !f.balancer.CanSendUniFrame(remainingLen) {
				f.StreamQueuePushBack(id)
				break
			}
		}

		// This should never return an error. Better check it anyway.
		// The stream will only be in the streamQueue, if it enqueued itself there.
		str, err := f.streamGetter.GetOrOpenSendStream(id)
		// The stream can be nil if it completed after it said it had data.
		if str == nil || err != nil {
			delete(f.activeStreams, id)
			continue
		}
		// For the last STREAM frame, we'll remove the DataLen field later.
		// Therefore, we can pretend to have more bytes available when popping
		// the STREAM frame (which will always have the DataLen set).
		remainingLen += quicvarint.Len(uint64(remainingLen))
		frame, ok, hasMoreData := str.popStreamFrame(remainingLen, v)
		if hasMoreData { // put the stream back in the queue (at the end)
			if f.balancer != nil {
				f.balancer.Debug("framer", "has more data")
			}
			f.StreamQueuePushBack(id)
		} else { // no more data to send. Stream is not active
			delete(f.activeStreams, id)
		}
		// The frame can be "nil"
		// * if the receiveStream was canceled after it said it had data
		// * the remaining size doesn't allow us to add another STREAM frame
		if !ok {
			continue
		}
		frames = append(frames, frame)
		lengthNewFrame := frame.Frame.Length(v)
		length += lengthNewFrame

		if f.balancer != nil {
			f.balancer.RegisterSentBytes(lengthNewFrame, id)
		}
	}
	f.mutex.Unlock()
	if len(frames) > startLen {
		l := frames[len(frames)-1].Frame.Length(v)
		// account for the smaller size of the last STREAM frame
		frames[len(frames)-1].Frame.DataLenPresent = false
		length += frames[len(frames)-1].Frame.Length(v) - l
	}
	if f.balancer != nil {
		f.balancer.Debug("framer:AppendStreamFrames", fmt.Sprintf("length: %d", length))
	}
	return frames, length
}

func (f *framerI) Handle0RTTRejection() error {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.controlFrameMutex.Lock()
	f.streamQueue.Clear()
	// f.bi_streamQueue.Clear()
	for id := range f.activeStreams {
		delete(f.activeStreams, id)
	}
	var j int
	for i, frame := range f.controlFrames {
		switch frame.(type) {
		case *wire.MaxDataFrame, *wire.MaxStreamDataFrame, *wire.MaxStreamsFrame:
			return errors.New("didn't expect MAX_DATA / MAX_STREAM_DATA / MAX_STREAMS frame to be sent in 0-RTT")
		case *wire.DataBlockedFrame, *wire.StreamDataBlockedFrame, *wire.StreamsBlockedFrame:
			continue
		default:
			f.controlFrames[j] = f.controlFrames[i]
			j++
		}
	}
	f.controlFrames = f.controlFrames[:j]
	f.controlFrameMutex.Unlock()
	return nil
}
