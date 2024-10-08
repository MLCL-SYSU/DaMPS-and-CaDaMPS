package ackhandler

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/lucas-clemente/quic-go/congestion"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/lucas-clemente/quic-go/internal/wire"
	"github.com/lucas-clemente/quic-go/qerr"
)

var rttArray []float64
var bandwidthArray []float64

var path1BandwidthArray []float64
var path2BandwidthArray []float64

const EWMAFactor = 0.5

const bandwidthLen = 10

const sessionBandwidthLen = 5

const (
	// Maximum reordering in time space before time based loss detection considers a packet lost.
	// In fraction of an RTT.
	timeReorderingFraction = 1.0 / 8
	// defaultRTOTimeout is the RTO time on new connections
	defaultRTOTimeout = 500 * time.Millisecond
	// Minimum time in the future an RTO alarm may be set for.
	minRTOTimeout = 200 * time.Millisecond
	// maxRTOTimeout is the maximum RTO time
	maxRTOTimeout = 60 * time.Second
	// Sends up to two tail loss probes before firing a RTO, as per
	// draft RFC draft-dukkipati-tcpm-tcp-loss-probe
	maxTailLossProbes = 2
	// TCP RFC calls for 1 second RTO however Linux differs from this default and
	// define the minimum RTO to 200ms, we will use the same until we have data to
	// support a higher or lower value
	minRetransmissionTime = 200 * time.Millisecond
	// Minimum tail loss probe time in ms
	minTailLossProbeTimeout = 10 * time.Millisecond
	// czy: discount reward factor gamma
	gamma      = 0.8
	batch      = 6
	historyLen = 5
)

var (
	// ErrDuplicateOrOutOfOrderAck occurs when a duplicate or an out-of-order ACK is received
	ErrDuplicateOrOutOfOrderAck = errors.New("SentPacketHandler: Duplicate or out-of-order ACK")
	// ErrTooManyTrackedSentPackets occurs when the sentPacketHandler has to keep track of too many packets
	ErrTooManyTrackedSentPackets = errors.New("Too many outstanding non-acked and non-retransmitted packets")
	// ErrAckForSkippedPacket occurs when the client sent an ACK for a packet number that we intentionally skipped
	ErrAckForSkippedPacket = qerr.Error(qerr.InvalidAckData, "Received an ACK for a skipped packet number")
	errAckForUnsentPacket  = qerr.Error(qerr.InvalidAckData, "Received ACK for an unsent package")
)

var errPacketNumberNotIncreasing = errors.New("Already sent a packet with a higher packet number")

type sentPacketHandler struct {
	lastSentPacketNumber protocol.PacketNumber
	skippedPackets       []protocol.PacketNumber

	numNonRetransmittablePackets int // number of non-retransmittable packets since the last retransmittable packet

	LargestAcked protocol.PacketNumber

	largestReceivedPacketWithAck protocol.PacketNumber

	packetHistory      *PacketList
	stopWaitingManager stopWaitingManager

	retransmissionQueue []*Packet

	bytesInFlight protocol.ByteCount

	congestion congestion.SendAlgorithm
	rttStats   *congestion.RTTStats

	onRTOCallback func(time.Time) bool

	// The number of times an RTO has been sent without receiving an ack.
	rtoCount uint32

	// The number of times a TLP has been sent without receiving an ACK
	tlpCount uint32

	// The time at which the next packet will be considered lost based on early transmit or exceeding the reordering window in time.
	lossTime time.Time

	// The time the last packet was sent, used to set the retransmission timeout
	lastSentTime time.Time

	// The alarm timeout
	alarm time.Time

	packets         uint64
	retransmissions uint64
	losses          uint64

	ackedBytes protocol.ByteCount
	sentBytes  protocol.ByteCount

	// czy:Change Point Detection Information
	changePDInfo ChangePointDetectionHandler

	curNotSent uint8 // save the current Not Sent

	// Dealine Meeting Ratio
	DeadlineRatio float32
}

type ChangePointDetectionHandler struct {
	totalMeetDeadline       uint16
	totalHasDeadline        uint16
	curMeetDeadline         uint16
	curHasDeadline          uint16
	alpha                   float32           // RTT discount factor, every RTT has an alpha
	historicalMeetDeadlines [][]uint16        // history curMeetDeadline
	historicalHasDeadlines  [][]uint16        // history curHasDeadline
	banditInformation       BanditInformation // Bandit Information
}

type BanditInformation struct {
	armsAlpha    []float32
	armsNumPlay  []int
	totalNumPlay int
	totalReward  []float32
	curArmIndex  int
}

// NewSentPacketHandler creates a new sentPacketHandler
func NewSentPacketHandler(rttStats *congestion.RTTStats, cong congestion.SendAlgorithm, onRTOCallback func(time.Time) bool) SentPacketHandler {
	var congestionControl congestion.SendAlgorithm

	if cong != nil {
		congestionControl = cong
	} else {
		congestionControl = congestion.NewCubicSender(
			congestion.DefaultClock{},
			rttStats,
			false, /* don't use reno since chromium doesn't (why?) */
			protocol.InitialCongestionWindow,
			protocol.DefaultMaxCongestionWindow,
		)
	}

	// initial BanditInformation
	bandit := NewBanditInformation()

	return &sentPacketHandler{
		packetHistory:      NewPacketList(),
		stopWaitingManager: stopWaitingManager{},
		rttStats:           rttStats,
		congestion:         congestionControl,
		onRTOCallback:      onRTOCallback,
		changePDInfo: ChangePointDetectionHandler{
			alpha:                   1.0,
			banditInformation:       bandit,
			historicalMeetDeadlines: make([][]uint16, len(bandit.armsAlpha)),
			historicalHasDeadlines:  make([][]uint16, len(bandit.armsAlpha)),
		},
	}
}

// NewBanditInformation creates a new BanditInformation
func NewBanditInformation() BanditInformation {
	var bandit BanditInformation
	bandit.armsAlpha = []float32{0.9, 1.0, 1.1, 1.2} // initial alpha
	bandit.armsNumPlay = []int{0, 0, 0, 0}           // initial num is zeros
	bandit.totalNumPlay = 0
	bandit.totalReward = []float32{0.0, 0.0, 0.0, 0.0} // initial total reward is zeros
	bandit.curArmIndex = 0
	return bandit
}

func (h *sentPacketHandler) GetStatistics() (uint64, uint64, uint64) {
	return h.packets, h.retransmissions, h.losses
}

func (h *sentPacketHandler) largestInOrderAcked() protocol.PacketNumber {
	if f := h.packetHistory.Front(); f != nil {
		return f.Value.PacketNumber - 1
	}
	return h.LargestAcked
}

func (h *sentPacketHandler) GetLastPackets() uint64 {
	return uint64(h.lastSentPacketNumber)
}

func (h *sentPacketHandler) GetPathAlpha() float32 {
	return h.changePDInfo.banditInformation.armsAlpha[h.changePDInfo.banditInformation.curArmIndex]
}

func (h *sentPacketHandler) ShouldSendRetransmittablePacket() bool {
	return h.numNonRetransmittablePackets >= protocol.MaxNonRetransmittablePackets
}

func (h *sentPacketHandler) SentPacket(packet *Packet) error {
	if packet.PacketNumber <= h.lastSentPacketNumber {
		return errPacketNumberNotIncreasing
	}

	if protocol.PacketNumber(len(h.retransmissionQueue)+h.packetHistory.Len()+1) > protocol.MaxTrackedSentPackets {
		return ErrTooManyTrackedSentPackets
	}

	for p := h.lastSentPacketNumber + 1; p < packet.PacketNumber; p++ {
		h.skippedPackets = append(h.skippedPackets, p)

		if len(h.skippedPackets) > protocol.MaxTrackedSkippedPackets {
			h.skippedPackets = h.skippedPackets[1:]
		}
	}

	h.lastSentPacketNumber = packet.PacketNumber
	now := time.Now()

	// Update some statistics
	h.packets++

	// XXX RTO and TLP are recomputed based on the possible last sent retransmission. Is it ok like this?
	h.lastSentTime = now

	packet.Frames = stripNonRetransmittableFrames(packet.Frames)
	isRetransmittable := len(packet.Frames) != 0
	//czy——deadline should be set in the function of packer_packet
	// if len(packet.Frames) != 0 {
	// 	packet.Deadline = time.Now().Add(rand.Intn(50)*time.Millisecond)
	// }

	if isRetransmittable {
		packet.SendTime = now
		h.bytesInFlight += packet.Length
		h.sentBytes += packet.Length
		h.packetHistory.PushBack(*packet)
		h.numNonRetransmittablePackets = 0
	} else {
		h.numNonRetransmittablePackets++
	}
	fmt.Println("Sent packet:", packet.PacketNumber, "with", packet.Length, "bytes", ". In sendtime:", packet.SendTime, ". Deadline:", packet.Deadline, ".")
	h.congestion.OnPacketSent(
		now,
		h.bytesInFlight,
		packet.PacketNumber,
		packet.Length,
		isRetransmittable,
	)

	h.updateLossDetectionAlarm()
	return nil
}

func (h *sentPacketHandler) ReceivedAck(ackFrame *wire.AckFrame, withPacketNumber protocol.PacketNumber,
	rcvTime time.Time) error {
	if ackFrame.LargestAcked > h.lastSentPacketNumber {
		return errAckForUnsentPacket
	}
	fmt.Println("received AckFrame:", ackFrame)
	fmt.Println("Meet Deadline packet number:", ackFrame.NumMeetDeadline)
	fmt.Println("All Deadline Packet number:", ackFrame.NumHasDeadline)
	fmt.Println("Receive Cur Not Sent:", ackFrame.CurNotSent)
	fmt.Println("Receive Alpha", ackFrame.Alpha)

	h.updateDeadlineInformation(ackFrame)

	// duplicate or out-of-order ACK
	if withPacketNumber <= h.largestReceivedPacketWithAck {
		return ErrDuplicateOrOutOfOrderAck
	}
	h.largestReceivedPacketWithAck = withPacketNumber

	// ignore repeated ACK (ACKs that don't have a higher LargestAcked than the last ACK)
	if ackFrame.LargestAcked <= h.largestInOrderAcked() {
		return nil
	}
	h.LargestAcked = ackFrame.LargestAcked

	if h.skippedPacketsAcked(ackFrame) {
		return ErrAckForSkippedPacket
	}

	rttUpdated := h.maybeUpdateRTT(ackFrame.LargestAcked, ackFrame.DelayTime, rcvTime)

	//olms: update bernoulliTrial
	if rttUpdated {
		// estimate bandwidth
		RTT := DurationToMilliseconds(h.rttStats.SmoothedRTT())

		// Calculate bandwidth from cwnd
		bandwidth := CwndToBandwidthMbps(float64(h.GetCongestionWindow()), RTT)
		h.updateSessionBandwidth(ackFrame.PathID, bandwidth)
		//h.lastReceivedTime = rcvTime

		// Session Bandwidth
		sB := calculateSessionBandwidth()

		// four path select two
		sB = sB / 2

		//// debug
		//if sB > 85 {
		//	sB = 85
		//}

		// Session RTT
		newSmoothRTT := computeSmoothRTT(RTT)
		AddRttArray(newSmoothRTT)

		// Bandwidth
		// smooth bandwidth
		//AddBandwidthArray(bandwidth)

		// Display rtt and bandwidth to save
		DisplayInformation(ackFrame.PathID, newSmoothRTT, bandwidth)
		DisplayDeadlineInfo(ackFrame.PathID, bandwidth, h.DeadlineRatio)
	}

	if rttUpdated {
		h.congestion.MaybeExitSlowStart()
	}

	ackedPackets, err := h.determineNewlyAckedPackets(ackFrame)
	if err != nil {
		return err
	}

	if len(ackedPackets) > 0 {
		for _, p := range ackedPackets {
			h.onPacketAcked(p)
			h.congestion.OnPacketAcked(p.Value.PacketNumber, p.Value.Length, h.bytesInFlight)
		}
	}

	h.detectLostPackets()
	h.updateLossDetectionAlarm()

	h.garbageCollectSkippedPackets()
	h.stopWaitingManager.ReceivedAck(ackFrame)

	return nil
}

func (h *sentPacketHandler) ReceivedClosePath(f *wire.ClosePathFrame, withPacketNumber protocol.PacketNumber, rcvTime time.Time) error {
	if f.LargestAcked > h.lastSentPacketNumber {
		return errAckForUnsentPacket
	}

	// this should never happen, since a closePath frame should be the last packet on a path
	if withPacketNumber <= h.largestReceivedPacketWithAck {
		return ErrDuplicateOrOutOfOrderAck
	}
	h.largestReceivedPacketWithAck = withPacketNumber

	// Compared to ACK frames, we should not ignore duplicate LargestAcked

	if h.skippedPacketsAckedClosePath(f) {
		return ErrAckForSkippedPacket
	}

	// No need for RTT estimation

	ackedPackets, err := h.determineNewlyAckedPacketsClosePath(f)
	if err != nil {
		return err
	}

	if len(ackedPackets) > 0 {
		for _, p := range ackedPackets {
			h.onPacketAcked(p)
			h.congestion.OnPacketAcked(p.Value.PacketNumber, p.Value.Length, h.bytesInFlight)
		}
	}

	h.SetInflightAsLost()

	h.garbageCollectSkippedPackets()
	// We do not send any STOP WAITING Frames, so no need to update the manager

	return nil
}

func (h *sentPacketHandler) determineNewlyAckedPackets(ackFrame *wire.AckFrame) ([]*PacketElement, error) {
	var ackedPackets []*PacketElement
	ackRangeIndex := 0
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		packet := el.Value
		packetNumber := packet.PacketNumber

		// Ignore packets below the LowestAcked
		if packetNumber < ackFrame.LowestAcked {
			continue
		}
		// Break after LargestAcked is reached
		if packetNumber > ackFrame.LargestAcked {
			break
		}

		if ackFrame.HasMissingRanges() {
			ackRange := ackFrame.AckRanges[len(ackFrame.AckRanges)-1-ackRangeIndex]

			for packetNumber > ackRange.Last && ackRangeIndex < len(ackFrame.AckRanges)-1 {
				ackRangeIndex++
				ackRange = ackFrame.AckRanges[len(ackFrame.AckRanges)-1-ackRangeIndex]
			}

			if packetNumber >= ackRange.First { // packet i contained in ACK range
				if packetNumber > ackRange.Last {
					return nil, fmt.Errorf("BUG: ackhandler would have acked wrong packet 0x%x, while evaluating range 0x%x -> 0x%x", packetNumber, ackRange.First, ackRange.Last)
				}
				ackedPackets = append(ackedPackets, el)
			}
		} else {
			ackedPackets = append(ackedPackets, el)
		}
	}

	return ackedPackets, nil
}

func (h *sentPacketHandler) determineNewlyAckedPacketsClosePath(f *wire.ClosePathFrame) ([]*PacketElement, error) {
	var ackedPackets []*PacketElement
	ackRangeIndex := 0
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		packet := el.Value
		packetNumber := packet.PacketNumber

		// Ignore packets below the LowestAcked
		if packetNumber < f.LowestAcked {
			continue
		}
		// Break after LargestAcked is reached
		if packetNumber > f.LargestAcked {
			break
		}

		if f.HasMissingRanges() {
			ackRange := f.AckRanges[len(f.AckRanges)-1-ackRangeIndex]

			for packetNumber > ackRange.Last && ackRangeIndex < len(f.AckRanges)-1 {
				ackRangeIndex++
				ackRange = f.AckRanges[len(f.AckRanges)-1-ackRangeIndex]
			}

			if packetNumber >= ackRange.First { // packet i contained in ACK range
				if packetNumber > ackRange.Last {
					return nil, fmt.Errorf("BUG: ackhandler would have acked wrong packet 0x%x, while evaluating range 0x%x -> 0x%x with ClosePath frame", packetNumber, ackRange.First, ackRange.Last)
				}
				ackedPackets = append(ackedPackets, el)
			}
		} else {
			ackedPackets = append(ackedPackets, el)
		}
	}

	return ackedPackets, nil
}

func (h *sentPacketHandler) maybeUpdateRTT(largestAcked protocol.PacketNumber, ackDelay time.Duration, rcvTime time.Time) bool {
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		packet := el.Value
		if packet.PacketNumber == largestAcked {
			h.rttStats.UpdateRTT(rcvTime.Sub(packet.SendTime), ackDelay, time.Now())
			return true
		}
		// Packets are sorted by number, so we can stop searching
		if packet.PacketNumber > largestAcked {
			break
		}
	}
	return false
}

func (h *sentPacketHandler) hasOutstandingRetransmittablePacket() bool {
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		if el.Value.IsRetransmittable() {
			return true
		}
	}
	return false
}

func (h *sentPacketHandler) updateLossDetectionAlarm() {
	// Cancel the alarm if no packets are outstanding
	if h.packetHistory.Len() == 0 {
		h.alarm = time.Time{}
		return
	}

	// TODO(#496): Handle handshake packets separately
	if !h.lossTime.IsZero() {
		// Early retransmit timer or time loss detection.
		h.alarm = h.lossTime
	} else if h.rttStats.SmoothedRTT() != 0 && h.tlpCount < maxTailLossProbes {
		// TLP
		h.alarm = h.lastSentTime.Add(h.computeTLPTimeout())
	} else {
		// RTO
		h.alarm = h.lastSentTime.Add(utils.MaxDuration(h.computeRTOTimeout(), minRetransmissionTime))
	}
}

func (h *sentPacketHandler) detectLostPackets() {
	h.lossTime = time.Time{}
	now := time.Now()

	maxRTT := float64(utils.MaxDuration(h.rttStats.LatestRTT(), h.rttStats.SmoothedRTT()))
	delayUntilLost := time.Duration((1.0 + timeReorderingFraction) * maxRTT)

	var lostPackets []*PacketElement
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		packet := el.Value

		if packet.PacketNumber > h.LargestAcked {
			break
		}

		timeSinceSent := now.Sub(packet.SendTime)
		if timeSinceSent > delayUntilLost {
			// Update statistics
			h.losses++
			lostPackets = append(lostPackets, el)
		} else if h.lossTime.IsZero() {
			// Note: This conditional is only entered once per call
			h.lossTime = now.Add(delayUntilLost - timeSinceSent)
		}
	}

	if len(lostPackets) > 0 {
		for _, p := range lostPackets {
			h.queuePacketForRetransmission(p)
			h.congestion.OnPacketLost(p.Value.PacketNumber, p.Value.Length, h.bytesInFlight)
		}
	}
}

func (h *sentPacketHandler) SetInflightAsLost() {
	var lostPackets []*PacketElement
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		packet := el.Value

		if packet.PacketNumber > h.LargestAcked {
			break
		}

		h.losses++
		lostPackets = append(lostPackets, el)
	}

	if len(lostPackets) > 0 {
		for _, p := range lostPackets {
			h.queuePacketForRetransmission(p)
			// XXX (QDC): should we?
			h.congestion.OnPacketLost(p.Value.PacketNumber, p.Value.Length, h.bytesInFlight)
		}
	}
}

func (h *sentPacketHandler) OnAlarm() {
	// Do we really have packet to retransmit?
	if !h.hasOutstandingRetransmittablePacket() {
		// Cancel then the alarm
		h.alarm = time.Time{}
		return
	}

	// TODO(#496): Handle handshake packets separately
	if !h.lossTime.IsZero() {
		// Early retransmit or time loss detection
		h.detectLostPackets()

	} else if h.tlpCount < maxTailLossProbes {
		// TLP
		h.retransmitTLP()
		h.tlpCount++
	} else {
		// RTO
		potentiallyFailed := false
		if h.onRTOCallback != nil {
			potentiallyFailed = h.onRTOCallback(h.lastSentTime)
		}
		if potentiallyFailed {
			h.retransmitAllPackets()
		} else {
			h.retransmitOldestTwoPackets()
		}
		h.rtoCount++
	}

	h.updateLossDetectionAlarm()
}

func (h *sentPacketHandler) GetAlarmTimeout() time.Time {
	return h.alarm
}

func (h *sentPacketHandler) GetAckedBytes() protocol.ByteCount {
	return h.ackedBytes
}

func (h *sentPacketHandler) GetSentBytes() protocol.ByteCount {
	return h.sentBytes
}

func (h *sentPacketHandler) GetCongestionWindow() protocol.ByteCount {
	return h.congestion.GetCongestionWindow()
}

func (h *sentPacketHandler) GetBytesInFlight() protocol.ByteCount {
	return h.bytesInFlight
}

func (h *sentPacketHandler) onPacketAcked(packetElement *PacketElement) {
	h.bytesInFlight -= packetElement.Value.Length
	h.rtoCount = 0
	h.tlpCount = 0
	h.packetHistory.Remove(packetElement)
	h.ackedBytes += packetElement.Value.Length
}

func (h *sentPacketHandler) DequeuePacketForRetransmission() *Packet {
	if len(h.retransmissionQueue) == 0 {
		return nil
	}
	packet := h.retransmissionQueue[0]
	// Shift the slice and don't retain anything that isn't needed.
	copy(h.retransmissionQueue, h.retransmissionQueue[1:])
	h.retransmissionQueue[len(h.retransmissionQueue)-1] = nil
	h.retransmissionQueue = h.retransmissionQueue[:len(h.retransmissionQueue)-1]
	// Update statistics
	h.retransmissions++
	return packet
}

func (h *sentPacketHandler) GetLeastUnacked() protocol.PacketNumber {
	return h.largestInOrderAcked() + 1
}

func (h *sentPacketHandler) GetStopWaitingFrame(force bool) *wire.StopWaitingFrame {
	return h.stopWaitingManager.GetStopWaitingFrame(force)
}

func (h *sentPacketHandler) SendingAllowed() bool {
	congestionLimited := h.bytesInFlight > h.congestion.GetCongestionWindow()
	maxTrackedLimited := protocol.PacketNumber(len(h.retransmissionQueue)+h.packetHistory.Len()) >= protocol.MaxTrackedSentPackets
	if congestionLimited {
		utils.Debugf("Congestion limited: bytes in flight %d, window %d",
			h.bytesInFlight,
			h.congestion.GetCongestionWindow())
	} else if maxTrackedLimited {
		utils.Debugf("Max tracked limited: %d",
			protocol.PacketNumber(len(h.retransmissionQueue)+h.packetHistory.Len()))
	}
	// Workaround for #555:
	// Always allow sending of retransmissions. This should probably be limited
	// to RTOs, but we currently don't have a nice way of distinguishing them.
	haveRetransmissions := len(h.retransmissionQueue) > 0
	//utils.Debugf("Is Allowed?: %t, max: %t, cong: %t, haveR: %t", !maxTrackedLimited && (!congestionLimited || haveRetransmissions), maxTrackedLimited, congestionLimited, haveRetransmissions)
	return !maxTrackedLimited && (!congestionLimited || haveRetransmissions)
}

func (h *sentPacketHandler) retransmitTLP() {
	if p := h.packetHistory.Back(); p != nil {
		h.queuePacketForRetransmission(p)
	}
}

func (h *sentPacketHandler) retransmitAllPackets() {
	for h.packetHistory.Len() > 0 {
		h.queueRTO(h.packetHistory.Front())
	}
	h.congestion.OnRetransmissionTimeout(true)
}

func (h *sentPacketHandler) retransmitOldestPacket() {
	if p := h.packetHistory.Front(); p != nil {
		h.queueRTO(p)
	}
}

func (h *sentPacketHandler) retransmitOldestTwoPackets() {
	h.retransmitOldestPacket()
	h.retransmitOldestPacket()
	h.congestion.OnRetransmissionTimeout(true)
}

func (h *sentPacketHandler) queueRTO(el *PacketElement) {
	packet := &el.Value
	utils.Debugf(
		"\tQueueing packet 0x%x for retransmission (RTO), %d outstanding",
		packet.PacketNumber,
		h.packetHistory.Len(),
	)
	h.queuePacketForRetransmission(el)
	h.losses++
	h.congestion.OnPacketLost(packet.PacketNumber, packet.Length, h.bytesInFlight)
}

func (h *sentPacketHandler) queuePacketForRetransmission(packetElement *PacketElement) {
	packet := &packetElement.Value
	h.bytesInFlight -= packet.Length
	h.retransmissionQueue = append(h.retransmissionQueue, packet)
	h.packetHistory.Remove(packetElement)
	h.stopWaitingManager.QueuedRetransmissionForPacketNumber(packet.PacketNumber)
}

func (h *sentPacketHandler) DuplicatePacket(packet *Packet) {
	h.retransmissionQueue = append(h.retransmissionQueue, packet)
}

func (h *sentPacketHandler) computeRTOTimeout() time.Duration {
	rto := h.congestion.RetransmissionDelay()
	if rto == 0 {
		rto = defaultRTOTimeout
	}
	rto = utils.MaxDuration(rto, minRTOTimeout)
	// Exponential backoff
	rto = rto << h.rtoCount
	return utils.MinDuration(rto, maxRTOTimeout)
}

func (h *sentPacketHandler) hasMultipleOutstandingRetransmittablePackets() bool {
	return h.packetHistory.Front() != nil && h.packetHistory.Front().Next() != nil
}

func (h *sentPacketHandler) computeTLPTimeout() time.Duration {
	rtt := h.congestion.SmoothedRTT()
	if h.hasMultipleOutstandingRetransmittablePackets() {
		return utils.MaxDuration(2*rtt, rtt*3/2+minRetransmissionTime/2)
	}
	return utils.MaxDuration(2*rtt, minTailLossProbeTimeout)
}

func (h *sentPacketHandler) skippedPacketsAcked(ackFrame *wire.AckFrame) bool {
	for _, p := range h.skippedPackets {
		if ackFrame.AcksPacket(p) {
			return true
		}
	}
	return false
}

func (h *sentPacketHandler) skippedPacketsAckedClosePath(closePathFrame *wire.ClosePathFrame) bool {
	for _, p := range h.skippedPackets {
		if closePathFrame.AcksPacket(p) {
			return true
		}
	}
	return false
}

func (h *sentPacketHandler) garbageCollectSkippedPackets() {
	lioa := h.largestInOrderAcked()
	deleteIndex := 0
	for i, p := range h.skippedPackets {
		if p <= lioa {
			deleteIndex = i + 1
		}
	}
	h.skippedPackets = h.skippedPackets[deleteIndex:]
}

func (h *sentPacketHandler) updateDeadlineInformation(ackFrame *wire.AckFrame) {
	h.changePDInfo.curMeetDeadline = ackFrame.NumMeetDeadline
	h.changePDInfo.curHasDeadline = ackFrame.NumHasDeadline

	// Find arm Index of alpha
	alphaTrue := float32(ackFrame.Alpha) / float32(10)
	armIndex := findIndexOfAlpha(alphaTrue, h.changePDInfo.banditInformation.armsAlpha)

	// Update history Deadline Information
	h.changePDInfo.updateHistoricalData(armIndex)

	// Update Bandit Information
	reward := h.CalculateHistoryMeetRatio(armIndex)
	h.DeadlineRatio = reward
	reward = reward - float32(ackFrame.CurNotSent)/float32(batch)
	h.changePDInfo.updateBanditInfo(reward, armIndex)

	// Update alpha
	h.changePDInfo.updateAlpha()

	// Update total Deadline Information
	h.changePDInfo.totalMeetDeadline = h.changePDInfo.totalMeetDeadline + ackFrame.NumMeetDeadline
	h.changePDInfo.totalHasDeadline = h.changePDInfo.totalHasDeadline + ackFrame.NumHasDeadline
	//fmt.Println("curMeetDeadline:", h.changePDInfo.curMeetDeadline)
	//fmt.Println("curHasDeadline:", h.changePDInfo.curHasDeadline)
	//fmt.Println("totalMeetDeadline:", h.changePDInfo.totalMeetDeadline)
	//fmt.Println("totalHasDeadline:", h.changePDInfo.totalHasDeadline)
}

func (cpd *ChangePointDetectionHandler) updateBanditInfo(reward float32, armIndex int) {
	// update reward
	//cpd.banditInformation.totalReward[cpd.banditInformation.curArmIndex] += reward
	fmt.Println("old total reward:", cpd.banditInformation.totalReward[armIndex])
	//update discount reward
	cpd.banditInformation.totalReward[armIndex] =
		gamma*cpd.banditInformation.totalReward[armIndex] + reward
	// update numPlays
	cpd.banditInformation.armsNumPlay[cpd.banditInformation.curArmIndex]++
	cpd.banditInformation.totalNumPlay++
}

func findIndexOfAlpha(alpha float32, arm []float32) int {
	index := 0
	for i, val := range arm {
		if isEqualFloat32(val, alpha) {
			index = i
		}
	}
	return index
}

func isEqualFloat32(a, b float32) bool {
	epsilon := 1e-5
	diff := math.Abs(float64(a) - float64(b))
	return diff < epsilon
}

func (cpd *ChangePointDetectionHandler) updateAlpha() {
	// computeUCB
	ucbs := cpd.banditInformation.computeUCB()

	//select best alpha
	bestArm := selectBestArm(ucbs)
	bestAlpha := cpd.banditInformation.armsAlpha[bestArm]
	cpd.banditInformation.curArmIndex = bestArm
	cpd.alpha = bestAlpha

	// print info
	//fmt.Println("ucbs:", ucbs)
	//fmt.Println("select arm:", bestArm)
	//fmt.Println("select alpha", bestAlpha)
}

func (bandit *BanditInformation) computeUCB() []float32 {
	ucbs := make([]float32, len(bandit.armsAlpha))
	for i := 0; i < len(bandit.armsAlpha); i++ {
		if bandit.armsNumPlay[i] == 0 {
			//ucbs[i] = float32(math.Inf(1))
			ucbs[i] = 2
		} else {
			aveReward := bandit.totalReward[i] / float32(bandit.armsNumPlay[i])
			delta := math.Sqrt(2 * math.Log(float64(bandit.totalNumPlay+1)) / float64(bandit.armsNumPlay[i]))
			ucbs[i] = aveReward + float32(delta)
		}
	}
	return ucbs
}

func selectBestArm(ucbs []float32) int {
	bestArm := 0
	maxUcb := ucbs[0]
	for i := 0; i < len(ucbs); i++ {
		if ucbs[i] > maxUcb {
			bestArm = i
			maxUcb = ucbs[i]
		}
	}
	return bestArm
}

func (cpd *ChangePointDetectionHandler) updateHistoricalData(armIndex int) {
	cpd.historicalMeetDeadlines[armIndex] = append(cpd.historicalMeetDeadlines[armIndex], cpd.curMeetDeadline)
	cpd.historicalHasDeadlines[armIndex] = append(cpd.historicalHasDeadlines[armIndex], cpd.curHasDeadline)

	//check slice is not more history len
	if len(cpd.historicalMeetDeadlines[armIndex]) > historyLen {
		cpd.historicalMeetDeadlines[armIndex] = cpd.historicalMeetDeadlines[armIndex][len(cpd.historicalMeetDeadlines[armIndex])-historyLen:]
	}

	if len(cpd.historicalHasDeadlines[armIndex]) > historyLen {
		cpd.historicalHasDeadlines[armIndex] = cpd.historicalHasDeadlines[armIndex][len(cpd.historicalHasDeadlines[armIndex])-historyLen:]
	}

	//fmt.Println("historyMeetDeadline:", cpd.historicalMeetDeadlines)
	//fmt.Println("historyHasDeadline:", cpd.historicalHasDeadlines)
}

//CalculateMeetRatio calculate accumulate meet ratio
func (h *sentPacketHandler) CalculateMeetRatio() float32 {
	curMeetRatio := float32(h.changePDInfo.totalMeetDeadline) / float32(h.changePDInfo.totalHasDeadline+1)
	return curMeetRatio
}

//CalculateInstantMeetRatio calculate instant meet ratio
func (h *sentPacketHandler) CalculateInstantMeetRatio() float32 {
	accumulateMeetRatio := h.CalculateMeetRatio()
	if h.changePDInfo.curHasDeadline == 0 {
		return 0
	} else {
		curMeetRatio := float32(h.changePDInfo.curMeetDeadline) / float32(h.changePDInfo.curHasDeadline)
		instantMeetRatio := accumulateMeetRatio*0.5 + curMeetRatio*0.5
		return instantMeetRatio
	}
}

//CalculateHistoryMeetRatio calculate history meet ratio
func (h *sentPacketHandler) CalculateHistoryMeetRatio(armIndex int) float32 {
	if len(h.changePDInfo.historicalMeetDeadlines[armIndex]) < historyLen ||
		len(h.changePDInfo.historicalHasDeadlines[armIndex]) < historyLen {
		return 0
	}

	var meetSum, hasSum uint16
	for i := len(h.changePDInfo.historicalMeetDeadlines[armIndex]) - historyLen; i < len(h.changePDInfo.historicalMeetDeadlines[armIndex]); i++ {
		meetSum += h.changePDInfo.historicalMeetDeadlines[armIndex][i]
		hasSum += h.changePDInfo.historicalHasDeadlines[armIndex][i]
	}

	if hasSum == uint16(0) {
		return 0 //if hasSum is zero, will divide by zero, and return NaN
	} else {
		return float32(meetSum) / float32(hasSum)
	}
}

func DurationToMilliseconds(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / float64(time.Millisecond)
}

func durationToSeconds(d time.Duration) float64 {
	return d.Seconds()
}

func CwndToBandwidthMbps(cwndBytes float64, timeSeconds float64) float64 {
	// 1 字节 (byte) = 8 比特 (bit)
	cwndBits := cwndBytes * 8

	// 1 兆比特 (Mbps) = 1,000,000 比特 (bits)
	cwndMbps := cwndBits / 1e6

	// cwnd 除以时间以获得 Mbps 传入的RTT为ms单位
	var cwndMbpsPerSecond float64
	if math.Abs(timeSeconds-0) > 1e-10 {
		cwndMbpsPerSecond = cwndMbps / (timeSeconds / 1e3)
	} else {
		cwndMbpsPerSecond = 0
	}

	return cwndMbpsPerSecond
}

func computeSmoothRTT(newRTT float64) float64 {
	numSamples := len(rttArray)
	var smoothRTT float64
	if numSamples == 0 {
		smoothRTT = newRTT
	} else {
		//fmt.Println("RTTArray:", rttArray[len(rttArray)-1])
		//fmt.Println("newRTT:", newRTT)
		smoothRTT = EWMAFactor*rttArray[len(rttArray)-1] + (1-EWMAFactor)*newRTT
	}
	return smoothRTT
}

func AddRttArray(newSmoothRTT float64) {
	rttArray = append(rttArray, newSmoothRTT)
}

func AddBandwidthArray(newBandwidth float64) {
	bandwidthArray = append(bandwidthArray, newBandwidth)
}

func DisplayInformation(pathID protocol.PathID, rtt, bandwidth float64) {
	fmt.Println("rtt(ms):", rtt)
	fmt.Println("pathID", pathID, ", bandwidth(Mbps):", bandwidth)
	fmt.Println(" ")
}

func DisplayDeadlineInfo(pathID protocol.PathID, bandwidth float64, deadlineRatio float32) {
	fmt.Println("pathID", pathID, ", deadline bandwidth(Mbps):", bandwidth*float64(deadlineRatio))
	fmt.Println(" ")
}

// computeBandwidth compute bandwidth follow ack rate like bbr. This function compute bandwidth with one sample
func computeBandwidth(largestACK, largestInOrderACK protocol.PacketNumber, rcvTime, lastACKTime time.Time) float64 {
	ackDelta := uint64(largestACK-largestInOrderACK) * uint64(protocol.MaxReceivePacketSize)
	timeDelta := rcvTime.Sub(lastACKTime).Seconds()        //second
	bandwidth := float64(ackDelta) * 8 / (timeDelta * 1e6) //Mbps

	fmt.Println("ackDelta:", ackDelta)
	fmt.Println("timeDelta:", timeDelta)
	fmt.Println("bandwidth(Mbps):", bandwidth)
	return bandwidth
}

func (h *sentPacketHandler) updateSessionBandwidth(pathID protocol.PathID, bandwidth float64) {
	if pathID == protocol.PathID(1) {
		path1BandwidthArray = append(path1BandwidthArray, bandwidth)
		if len(path1BandwidthArray) > sessionBandwidthLen {
			path1BandwidthArray = path1BandwidthArray[len(path1BandwidthArray)-sessionBandwidthLen:]
		}
	} else if pathID == protocol.PathID(3) {
		path2BandwidthArray = append(path2BandwidthArray, bandwidth)
		if len(path2BandwidthArray) > sessionBandwidthLen {
			path2BandwidthArray = path2BandwidthArray[len(path2BandwidthArray)-sessionBandwidthLen:]
		}
	}
}

func calculateSessionBandwidth() float64 {
	sumBandwidth := 0.0
	for _, value := range path1BandwidthArray {
		sumBandwidth += value
	}

	for _, value := range path2BandwidthArray {
		sumBandwidth += value
	}

	sessionB := sumBandwidth / sessionBandwidthLen
	return sessionB
}
