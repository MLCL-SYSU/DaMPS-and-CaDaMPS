package quic

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/lucas-clemente/quic-go/ackhandler"
	"github.com/lucas-clemente/quic-go/internal/handshake"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/wire"
)

type packedPacket struct {
	number          protocol.PacketNumber
	raw             []byte
	frames          []wire.Frame
	encryptionLevel protocol.EncryptionLevel
	//czy
	m_deadline time.Time
}

type packetPacker struct {
	connectionID protocol.ConnectionID
	perspective  protocol.Perspective
	version      protocol.VersionNumber
	cryptoSetup  handshake.CryptoSetup

	connectionParameters handshake.ConnectionParametersManager
	streamFramer         *streamFramer

	controlFrames []wire.Frame
	stopWaiting   map[protocol.PathID]*wire.StopWaitingFrame
	ackFrame      map[protocol.PathID]*wire.AckFrame
}

func newPacketPacker(connectionID protocol.ConnectionID,
	cryptoSetup handshake.CryptoSetup,
	connectionParameters handshake.ConnectionParametersManager,
	streamFramer *streamFramer,
	perspective protocol.Perspective,
	version protocol.VersionNumber,
) *packetPacker {
	return &packetPacker{
		cryptoSetup:          cryptoSetup,
		connectionID:         connectionID,
		connectionParameters: connectionParameters,
		perspective:          perspective,
		version:              version,
		streamFramer:         streamFramer,
		stopWaiting:          make(map[protocol.PathID]*wire.StopWaitingFrame),
		ackFrame:             make(map[protocol.PathID]*wire.AckFrame),
	}
}

// PackConnectionClose packs a packet that ONLY contains a ConnectionCloseFrame
func (p *packetPacker) PackConnectionClose(ccf *wire.ConnectionCloseFrame, pth *path) (*packedPacket, error) {
	frames := []wire.Frame{ccf}
	encLevel, sealer := p.cryptoSetup.GetSealer()
	ph := p.getPublicHeader(encLevel, pth)
	raw, err := p.writeAndSealPacket(ph, frames, sealer, pth)
	fmt.Println("PackConnectionClose--contains a ConnectionCloseFrame")
	return &packedPacket{
		number:          ph.PacketNumber,
		raw:             raw,
		frames:          frames,
		encryptionLevel: encLevel,
	}, err
}

// PackPing packs a packet that ONLY contains a PingFrame
func (p *packetPacker) PackPing(pf *wire.PingFrame, pth *path) (*packedPacket, error) {
	fmt.Println("PackPing--contains a PingFrame")
	// Add the PingFrame in front of the controlFrames
	pth.SetLeastUnacked(pth.sentPacketHandler.GetLeastUnacked())
	p.controlFrames = append([]wire.Frame{pf}, p.controlFrames...)
	//czy:this deadline is no value
	var deadline time.Time
	fmt.Println("PackPing--deadline:", deadline)
	curNotSent := uint8(0)
	return p.PackPacket(pth, deadline, curNotSent, uint8(1))
}

func (p *packetPacker) PackAckPacket(pth *path) (*packedPacket, error) {
	fmt.Println("PackAckPacket--contains a AckFrame")
	if p.ackFrame[pth.pathID] == nil {
		return nil, errors.New("packet packer BUG: no ack frame queued")
	}
	encLevel, sealer := p.cryptoSetup.GetSealer()
	ph := p.getPublicHeader(encLevel, pth)
	frames := []wire.Frame{p.ackFrame[pth.pathID]}
	fmt.Println("ackFrame:", p.ackFrame[pth.pathID])
	//fmt.Println("frames:", frames)
	if p.stopWaiting[pth.pathID] != nil {
		p.stopWaiting[pth.pathID].PacketNumber = ph.PacketNumber
		p.stopWaiting[pth.pathID].PacketNumberLen = ph.PacketNumberLen
		frames = append(frames, p.stopWaiting[pth.pathID])
		p.stopWaiting[pth.pathID] = nil
	}
	p.ackFrame[pth.pathID] = nil
	raw, err := p.writeAndSealPacket(ph, frames, sealer, pth)
	return &packedPacket{
		number:          ph.PacketNumber,
		raw:             raw,
		frames:          frames,
		encryptionLevel: encLevel,
	}, err
}

// PackHandshakeRetransmission retransmits a handshake packet, that was sent with less than forward-secure encryption
func (p *packetPacker) PackHandshakeRetransmission(packet *ackhandler.Packet, pth *path) (*packedPacket, error) {
	fmt.Println("PackHandshakeRetransmission--contains a StopWaitingFrame")
	if packet.EncryptionLevel == protocol.EncryptionForwardSecure {
		return nil, errors.New("PacketPacker BUG: forward-secure encrypted handshake packets don't need special treatment")
	}
	sealer, err := p.cryptoSetup.GetSealerWithEncryptionLevel(packet.EncryptionLevel)
	if err != nil {
		return nil, err
	}
	if p.stopWaiting[pth.pathID] == nil {
		return nil, errors.New("PacketPacker BUG: Handshake retransmissions must contain a StopWaitingFrame")
	}
	ph := p.getPublicHeader(packet.EncryptionLevel, pth)
	p.stopWaiting[pth.pathID].PacketNumber = ph.PacketNumber
	p.stopWaiting[pth.pathID].PacketNumberLen = ph.PacketNumberLen
	frames := append([]wire.Frame{p.stopWaiting[pth.pathID]}, packet.Frames...)
	p.stopWaiting[pth.pathID] = nil
	raw, err := p.writeAndSealPacket(ph, frames, sealer, pth)
	return &packedPacket{
		number:          ph.PacketNumber,
		raw:             raw,
		frames:          frames,
		encryptionLevel: packet.EncryptionLevel,
	}, err
}

// PackPacket packs a new packet
// the other controlFrames are sent in the next packet, but might be queued and sent in the next packet if the packet would overflow MaxPacketSize otherwise
func (p *packetPacker) PackPacket(pth *path, deadline time.Time, curNotSent uint8, alpha uint8) (*packedPacket, error) {
	fmt.Println("PackPacket!")
	if p.streamFramer.HasCryptoStreamFrame() {
		return p.packCryptoPacket(pth)
	}

	encLevel, sealer := p.cryptoSetup.GetSealer()

	//czy:Add deadline to publicHeader(encLevel, pth, deadline)
	publicHeader := p.getPublicHeader(encLevel, pth)
	//czy
	publicHeader.Deadline = deadline
	publicHeader.CurNotSent = curNotSent
	publicHeader.Alpha = alpha

	publicHeaderLength, err := publicHeader.GetLength(p.perspective)
	if err != nil {
		fmt.Println("GetLength error!")
		return nil, err
	}
	if p.stopWaiting[pth.pathID] != nil {
		p.stopWaiting[pth.pathID].PacketNumber = publicHeader.PacketNumber
		p.stopWaiting[pth.pathID].PacketNumberLen = publicHeader.PacketNumberLen
	}

	// TODO (QDC): rework this part with PING
	var isPing bool
	if len(p.controlFrames) > 0 {
		_, isPing = p.controlFrames[0].(*wire.PingFrame)
	}

	var payloadFrames []wire.Frame
	if isPing {
		payloadFrames = []wire.Frame{p.controlFrames[0]}
		// Remove the ping frame from the control frames
		p.controlFrames = p.controlFrames[1:len(p.controlFrames)]
	} else {
		maxSize := protocol.MaxPacketSize - protocol.ByteCount(sealer.Overhead()) - publicHeaderLength
		payloadFrames, err = p.composeNextPacket(maxSize, p.canSendData(encLevel), pth)
		if err != nil {
			fmt.Println("composeNextPacket error!")
			return nil, err
		}
	}

	// Check if we have enough frames to send
	if len(payloadFrames) == 0 {
		fmt.Println("payloadFrame is 0.")
		return nil, nil
	}
	// Don't send out packets that only contain a StopWaitingFrame
	if len(payloadFrames) == 1 && p.stopWaiting[pth.pathID] != nil {
		fmt.Println("contain a StopWaitingFrame.")
		return nil, nil
	}
	p.stopWaiting[pth.pathID] = nil
	p.ackFrame[pth.pathID] = nil

	//czy:将包头和payload写成数据raw （byte）
	raw, err := p.writeAndSealPacket(publicHeader, payloadFrames, sealer, pth)
	if err != nil {
		fmt.Println("writeAndSeadPacket error!")
		return nil, err
	}
	//only packet with frames can be add deadline
	return &packedPacket{
		number:          publicHeader.PacketNumber,
		raw:             raw,
		frames:          payloadFrames,
		encryptionLevel: encLevel,
		m_deadline:      deadline,
	}, nil
}

func (p *packetPacker) packCryptoPacket(pth *path) (*packedPacket, error) {
	encLevel, sealer := p.cryptoSetup.GetSealerForCryptoStream()
	publicHeader := p.getPublicHeader(encLevel, pth)
	publicHeaderLength, err := publicHeader.GetLength(p.perspective)
	if err != nil {
		return nil, err
	}
	maxLen := protocol.MaxPacketSize - protocol.ByteCount(sealer.Overhead()) - protocol.NonForwardSecurePacketSizeReduction - publicHeaderLength
	frames := []wire.Frame{p.streamFramer.PopCryptoStreamFrame(maxLen)}
	raw, err := p.writeAndSealPacket(publicHeader, frames, sealer, pth)
	if err != nil {
		return nil, err
	}
	fmt.Println("packCryptoPacket--PacketNumber:", publicHeader.PacketNumber)
	return &packedPacket{
		number:          publicHeader.PacketNumber,
		raw:             raw,
		frames:          frames,
		encryptionLevel: encLevel,
	}, nil
}

func (p *packetPacker) composeNextPacket(
	maxFrameSize protocol.ByteCount,
	canSendStreamFrames bool,
	pth *path,
) ([]wire.Frame, error) {
	var payloadLength protocol.ByteCount
	var payloadFrames []wire.Frame

	// STOP_WAITING and ACK will always fit
	if p.stopWaiting[pth.pathID] != nil {
		fmt.Println("stopWaiting is not nil.")
		payloadFrames = append(payloadFrames, p.stopWaiting[pth.pathID])
		l, err := p.stopWaiting[pth.pathID].MinLength(p.version)
		if err != nil {
			return nil, err
		}
		payloadLength += l
	}
	if p.ackFrame[pth.pathID] != nil {
		fmt.Println("ackFrame is not nil.")
		payloadFrames = append(payloadFrames, p.ackFrame[pth.pathID])
		l, err := p.ackFrame[pth.pathID].MinLength(p.version)
		if err != nil {
			return nil, err
		}
		payloadLength += l
	}

	for len(p.controlFrames) > 0 {
		frame := p.controlFrames[len(p.controlFrames)-1]
		minLength, err := frame.MinLength(p.version)
		if err != nil {
			return nil, err
		}
		if payloadLength+minLength > maxFrameSize {
			break
		}
		payloadFrames = append(payloadFrames, frame)
		payloadLength += minLength
		p.controlFrames = p.controlFrames[:len(p.controlFrames)-1]
	}

	if payloadLength > maxFrameSize {
		return nil, fmt.Errorf("Packet Packer BUG: packet payload (%d) too large (%d)", payloadLength, maxFrameSize)
	}

	if !canSendStreamFrames {
		return payloadFrames, nil
	}

	// temporarily increase the maxFrameSize by 2 bytes
	// this leads to a properly sized packet in all cases, since we do all the packet length calculations with StreamFrames that have the DataLen set
	// however, for the last StreamFrame in the packet, we can omit the DataLen, thus saving 2 bytes and yielding a packet of exactly the correct size
	maxFrameSize += 2

	fs := p.streamFramer.PopStreamFrames(maxFrameSize - payloadLength)
	if len(fs) != 0 {
		fs[len(fs)-1].DataLenPresent = false
	}

	// TODO: Simplify
	for _, f := range fs {
		payloadFrames = append(payloadFrames, f)
	}

	for b := p.streamFramer.PopBlockedFrame(); b != nil; b = p.streamFramer.PopBlockedFrame() {
		p.controlFrames = append(p.controlFrames, b)
	}

	return payloadFrames, nil
}

func (p *packetPacker) QueueControlFrame(frame wire.Frame, pth *path) {
	switch f := frame.(type) {
	case *wire.StopWaitingFrame:
		p.stopWaiting[pth.pathID] = f
	case *wire.AckFrame:
		p.ackFrame[pth.pathID] = f
	default:
		p.controlFrames = append(p.controlFrames, f)
	}
}

func (p *packetPacker) getPublicHeader(encLevel protocol.EncryptionLevel, pth *path) *wire.PublicHeader {
	pnum := pth.packetNumberGenerator.Peek()
	packetNumberLen := protocol.GetPacketNumberLengthForPublicHeader(pnum, pth.leastUnacked)
	publicHeader := &wire.PublicHeader{
		ConnectionID:         p.connectionID,
		PacketNumber:         pnum,
		PacketNumberLen:      packetNumberLen,
		TruncateConnectionID: p.connectionParameters.TruncateConnectionID(),
	}

	if p.perspective == protocol.PerspectiveServer && encLevel == protocol.EncryptionSecure {
		publicHeader.DiversificationNonce = p.cryptoSetup.DiversificationNonce()
	}
	if p.perspective == protocol.PerspectiveClient && encLevel != protocol.EncryptionForwardSecure {
		publicHeader.VersionFlag = true
		publicHeader.VersionNumber = p.version
	}

	// XXX (QDC): need a additional check because of tests
	if pth.sess != nil && pth.sess.handshakeComplete && p.version >= protocol.VersionMP {
		publicHeader.MultipathFlag = true
		publicHeader.PathID = pth.pathID
		// XXX (QDC): in case of doubt, never truncate the connection ID. This might change...
		publicHeader.TruncateConnectionID = false
	}

	return publicHeader
}

func (p *packetPacker) writeAndSealPacket(
	publicHeader *wire.PublicHeader,
	payloadFrames []wire.Frame,
	sealer handshake.Sealer,
	pth *path,
) ([]byte, error) {
	raw := getPacketBuffer()
	buffer := bytes.NewBuffer(raw)

	if err := publicHeader.Write(buffer, p.version, p.perspective); err != nil {
		return nil, err
	}
	payloadStartIndex := buffer.Len()

	for _, frame := range payloadFrames {
		//fmt.Println("Write Frame", frame)
		err := frame.Write(buffer, p.version)
		if err != nil {
			return nil, err
		}
	}
	//fmt.Println("buffer-header+frame:", buffer.Bytes())
	if protocol.ByteCount(buffer.Len()+sealer.Overhead()) > protocol.MaxPacketSize {
		return nil, errors.New("PacketPacker BUG: packet too large")
	}

	raw = raw[0:buffer.Len()]
	_ = sealer.Seal(raw[payloadStartIndex:payloadStartIndex], raw[payloadStartIndex:], publicHeader.PacketNumber, raw[:payloadStartIndex])
	raw = raw[0 : buffer.Len()+sealer.Overhead()]
	//fmt.Println("sealer.Overhead()", sealer.Overhead())

	num := pth.packetNumberGenerator.Pop()
	if num != publicHeader.PacketNumber {
		return nil, errors.New("packetPacker BUG: Peeked and Popped packet numbers do not match")
	}
	//fmt.Println("In writeAndSealPacket, all raw:", raw)
	return raw, nil
}

func (p *packetPacker) canSendData(encLevel protocol.EncryptionLevel) bool {
	if p.perspective == protocol.PerspectiveClient {
		return encLevel >= protocol.EncryptionSecure
	}
	return encLevel == protocol.EncryptionForwardSecure
}
