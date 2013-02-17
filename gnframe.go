// Copyright 2013 Michael Meier. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Package gnc offers communication routines for use with gritnote systems.
package gnc

import (
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

const (
	DEFAULT_FRAMELEN    = 128
	DEFAULT_SYNCTIMEOUT = 1 * time.Second
)

// GNFrameTransceiver provides a receiver and a transmitter for gnframes, the
// unit of transmission in a synchronized gritnote system. Frames are of the
// following format.
// 		[0x80] [framelen] [payload[0]] [payload[1]] ... [payload[n-1]] [checksum]
// Where [x] denotes an unsigned byte-sized datum with value x. The frame
// contains n payload bytes. Counting the sync byte 0x80, the frame length byte
// framelen and the checksum, this makes for framelen == n+3. Thus, a maximum
// of 252 payload bytes are supported. The checksum is the ones' complement sum
// of the sync byte 0x80, the framelen and all the payload bytes.
//
// The maximum frame length supported by the Transceiver can be configured
// via the SetMaxFrameLen method. The Read methods may not be called
// concurrently.
type GNFrameTransceiver struct {
	c           io.ReadWriter
	maxframelen int
	synctimeout time.Duration
	parammutex  sync.Mutex
	rscratch    [4]byte // scratch storage for receiving frames
}

func NewGNFrameTransceiver(c io.ReadWriter) *GNFrameTransceiver {
	var t GNFrameTransceiver
	t.c = c
	t.maxframelen = DEFAULT_FRAMELEN
	t.synctimeout = DEFAULT_SYNCTIMEOUT

	return &t
}

// SetMaxFrameLen sets the maximum frame length in use by this Transceiver.
// The permissible range for mfl is 3 <= mfl <= 252. The maximum payload size
// will be mfl - 3.
func (t *GNFrameTransceiver) SetMaxFrameLen(mfl int) error {
	t.parammutex.Lock()
	defer t.parammutex.Unlock()

	if mfl < 3 {
		return errors.New("SetMaxFrameLen: max frame len needs to be at lest 3 bytes")
	}

	if mfl > 255 {
		return errors.New("SetMaxFrameLen: max frame len may not exceed 255 bytes")
	}

	t.maxframelen = mfl

	return nil
}

func (t *GNFrameTransceiver) GetMaxFrameLen() int {
	t.parammutex.Lock()
	defer t.parammutex.Unlock()
	return t.maxframelen
}

// Write sends the buffer b wrapped in a gnframe. If b is too long for
// transmission, an error is returned.
func (t *GNFrameTransceiver) Write(b []byte) (int, error) {
	if (len(b) + 3) > t.maxframelen {
		return 0, fmt.Errorf("GNFrameTransceiver.Write: passed buffer plus 3 bytes overhead exceed %d bytes maximum frame length", t.GetMaxFrameLen())
	}

	sblen := len(b) + 3

	// we do an extraneous allocation and copy, but avoid 2 syscalls
	// i'm not aware of an elegant way to solve this dilemma
	sb := make([]byte, sblen)
	copy(sb[2:], b)
	sb[0] = 0x80
	sb[1] = uint8(sblen)
	sb[sblen-1] = onec_sum(sb[:sblen-1])

	log.Printf("sending buffer % x\n", sb)

	return t.c.Write(sb)
}

// ReadFrame reads one gncomm frame from the underlying connection. If, while
// receiving a frame from the underlying connection, a timeout occurs,
// it is passed to the caller as a Timeout. If a timeout occurs on the
// underlying connection while the Transceiver is looking for a start of frame
// marker, a NoFrameReceivedError is returned. If no synchronization character
// is received within DEFAULT_SYNCTIMEOUT, a NoFrameReceivedError is returned.
func (t *GNFrameTransceiver) ReadFrame() ([]byte, error) {
	rb, _, err := t.readFrame(nil)
	return rb, err
}

// Read reads one gncomm frame from the underlying connection into the provided
// buffer rb. Its error semantics are analogous to those of ReadFrame.
// If the buffer is too small to hold the frame, only the first
// len(rb) bytes are read into rb. The remaining bytes are discarded and an
// error is returned.
func (t *GNFrameTransceiver) Read(rb []byte) (int, error) {
	nrb, n, err := t.readFrame(rb)
	if n > len(rb) {
		copy(rb, nrb[0:len(rb)])
		return 0, fmt.Errorf("receive GNFrame: %d bytes of payload too long for slice of length %d", n, len(rb))
	}
	return n, err
}

func (t *GNFrameTransceiver) readFrame(srb []byte) ([]byte, int, error) {
	// t.rscratch is used to hold the frame's non payload data
	// t.rscratch[0] holds the SOF character, 0x80, index 1 holds
	// the frame length byte and index 2 holds the checksum
	log.Printf("gnxcvr: getting start of frame")
	// get start of frame

	tstart := time.Now()

	// TODO: configurable timeout/sync error behaviour. currently timeouts
	// and framing errors are silently ignored.
	// TODO: what to do in the case of our peer constantly transmitting
	// non-0x80 bytes?
	for {
		_, err := t.c.Read(t.rscratch[0:1])
		log.Printf("err %v", err)
		if err != nil {
			if isTimeout(err) {
				return nil, 0, noframe
			}

			return nil, 0, err
		}

		if t.rscratch[0] != 0x80 {
			// we're not getting in sync
			if time.Now().Sub(tstart) > t.synctimeout {
				return nil, 0, noframe
			}

			continue
		}

		break
	}

	log.Printf("gnxcvr: getting length")

	// we've got a start of frame, read len
	_, err := t.c.Read(t.rscratch[1:2])
	if err != nil {
		return nil, 0, err
	}
	framelen := int(t.rscratch[1])
	payloadlen := framelen - 3

	log.Printf("gnxcvr: got framelen %#02x", framelen)

	if framelen > t.GetMaxFrameLen() {
		// frame too long for this transceiver
		// the current behaviour is to lose sync, halt and catch fire
		// TODO: dummy read the frame to stay in sync
		// and return appropriate error to caller
		return nil, 0, errors.New("remote host sent a frame too large for this transceiver")
	}

	if framelen < 3 {
		return nil, 0, fmt.Errorf("remote host indicates frame len %d, too short to be a real frame", framelen)
	}

	// in case the user passes a slice which has not enough room for the frame,
	// allocate a new buffer
	rb := srb
	if payloadlen > len(rb) {
		log.Printf("gnxcvr: srb len %d too short, allocating new slice", len(srb))
		rb = make([]byte, payloadlen)
	}

	// read payload	
	nread := 0
	for nread < payloadlen {
		n, err := t.c.Read(rb[nread:payloadlen])
		if err != nil {
			return nil, 0, err
		}

		nread += n
	}

	log.Printf("gnxcvr: getting checksum")

	// read checksum
	_, err = t.c.Read(t.rscratch[2:3])
	if err != nil {
		return nil, nread, err
	}

	log.Printf("got t.rscratch[2:3] % x", t.rscratch[2:3])

	log.Printf("gnxcvr: rscratch[0:3] % x, rb[0:payloadlen] % x", t.rscratch[0:3], rb[0:payloadlen])

	var s uint8 = 0
	s = onec_add(t.rscratch[0], t.rscratch[1])
	s = onec_add(s, onec_sum(rb[0:nread]))

	log.Printf("gnxcvr: frame checksum is %#02x, our checksum is %#02x", t.rscratch[2], s)

	if s != t.rscratch[2] {
		return rb, nread, errors.New("GNFrameTransceiver.Read: checksum mismatch")
	}

	return rb, nread, nil
}

type Timeout interface {
	Timeout() bool
}

func isTimeout(e error) bool {
	if to, ok := e.(Timeout); ok {
		return to.Timeout()
	}
	return false
}

// NoFrameReceivedError signifies that no frame synchronization has been
// received within certain timeouts. 
//
// For example, when ReadFrame is called and the line stays idle for
// the specified timeout periods, a NoFrameReceivedError is returned.
// A NoFrameReceivedError is also returned when, during the specified
// timeouts no synchronization character is received. This situation may
// arise in a number of ways, the most prominent ones being baud rate
// mismatch and the peer not using gnframes.
type NoFrameReceivedError interface {
	error
	NoFrameReceived() bool
}

type noframe_t string

func (n noframe_t) Error() string         { return string(n) }
func (n noframe_t) NoFrameReceived() bool { return true }

var noframe = noframe_t("no gnframe received in sync tieout")

func IsNoFrameReceivedError(e error) bool {
	if nf, ok := e.(NoFrameReceivedError); ok {
		return nf.NoFrameReceived()
	}
	return false
}

func onec_add(a, b uint8) uint8 {
	s := uint(a) + uint(b)
	if (s & 0x100) != 0 {
		return uint8((s & 0xff) + 1)
	}
	return uint8(s)
}

func onec_sum(b []uint8) uint8 {
	var s uint = 0
	for _, x := range b {
		s += uint(x)
		if (s & 0x100) != 0 {
			s = (s & 0xff) + 1
		}
	}
	return uint8(s)
}
