package main

// receiver is a bit-at-a-time CAN 2.0A decoder. It performs destuffing while
// collecting SOF..CRC, then validates the fixed post-CRC fields.
type receiver struct {
	targetRaw int // raw SOF..CRC length, initially DLC 0's length
	raw       []BitLevel

	// Stuff-region state.
	lastRaw       BitLevel // last non-stuffed (raw) bit
	run           int      // consecutive identical raw bits since the last stuff bit
	afterStuff    bool     // next raw bit follows a consumed stuff bit
	expectStuff   bool     // next physical bit must be a complement stuff bit
	finishAtStuff bool     // the expected stuff bit is the frame's final bit
	stuffError    bool

	complete         bool
	frame            Frame
	dlc              int
	crc              int
	nextBitError     string // signalled at the following physical bit (stuff error)
	immediatePending bool   // form error on the bit just consumed
	crcError         bool   // signalled at the ACK delimiter
	formError        bool
}

func newReceiver() *receiver {
	r := &receiver{}
	r.reset()
	return r
}

func (r *receiver) reset() {
	r.targetRaw = 34 // Updated once the four DLC raw bits have been decoded.
	r.raw = nil
	r.lastRaw = Recessive
	r.run = 0
	r.afterStuff = false
	r.expectStuff = false
	r.finishAtStuff = false
	r.stuffError = false
	r.complete = false
	r.frame = Frame{}
	r.dlc = 0
	r.crc = 0
	r.nextBitError = ""
	r.immediatePending = false
	r.crcError = false
	r.formError = false
}

// consumeNextBitError returns and clears an error that must begin on the bit
// following the one where it was detected.
func (r *receiver) consumeNextBitError() string {
	err := r.nextBitError
	r.nextBitError = ""
	return err
}

// takeImmediateError reports whether the bit just consumed requires an error
// flag starting immediately at the current wire bit.
func (r *receiver) takeImmediateError() bool {
	v := r.immediatePending
	r.immediatePending = false
	return v
}

// canAck reports whether this receiver should drive the ACK slot dominant.
// Acceptance filtering is deliberately not considered here.
func (r *receiver) canAck() bool {
	return r.complete && !r.crcError && !r.formError && r.nextBitError == ""
}

func (r *receiver) delivered() bool {
	return r.complete && !r.crcError && !r.formError
}

func (r *receiver) failNextBit(kind string) {
	if r.nextBitError == "" {
		r.nextBitError = kind
	}
}

// feed consumes one expected physical frame bit.
func (r *receiver) feed(level BitLevel, kind BitKind) {
	if r.stuffError {
		return
	}
	if !r.complete {
		r.feedStuffed(level, kind)
		return
	}
	r.feedFixed(level, kind)
}

func (r *receiver) feedStuffed(level BitLevel, kind BitKind) {
	if r.expectStuff {
		r.expectStuff = false
		if kind != KindStuff || level == r.lastRaw {
			r.stuffError = true
			r.failNextBit(reasonStuff)
			return
		}
		// The complement is not appended. The next raw bit starts a fresh run
		// of one, even if it equals the pre-stuff level.
		r.afterStuff = true
		r.run = 0
		if r.finishAtStuff {
			r.finishAtStuff = false
			r.finishStuffed()
		}
		return
	}

	if kind == KindStuff {
		r.stuffError = true
		r.failNextBit(reasonStuff)
		return
	}

	if r.afterStuff || r.run == 0 || level != r.lastRaw {
		r.lastRaw = level
		r.run = 1
		r.afterStuff = false
	} else {
		r.run++
	}
	r.raw = append(r.raw, level)

	if len(r.raw) == 19 {
		r.dlc = bitsToInt(r.raw[15:19])
		if r.dlc > 8 {
			r.formError = true
			r.immediatePending = true
			return
		}
		r.targetRaw = 34 + 8*r.dlc
	}

	if len(r.raw) == r.targetRaw {
		if r.run == 5 {
			// A terminal run of five still requires its complement before the
			// CRC delimiter.
			r.expectStuff = true
			r.finishAtStuff = true
		} else {
			r.finishStuffed()
		}
	} else if r.run == 5 {
		r.expectStuff = true
	}
}

func (r *receiver) finishStuffed() {
	r.complete = true
	r.frame.ID = bitsToInt(r.raw[1:12])
	r.frame.Data = nil
	for off := 0; off < r.dlc; off++ {
		start := 19 + off*8
		r.frame.Data = append(r.frame.Data, byte(bitsToInt(r.raw[start:start+8])))
	}
	r.crc = bitsToInt(r.raw[r.targetRaw-15 : r.targetRaw])
	if r.crc != crc15(r.raw[:r.targetRaw-15]) {
		// CRC failures are signalled in the ACK delimiter. Such a receiver also
		// stays recessive in the ACK slot.
		r.crcError = true
	}
}

func (r *receiver) feedFixed(level BitLevel, kind BitKind) {
	expectRecessive := kind == KindCRCDelim ||
		kind == KindACKDelim ||
		kind == KindEOF
	if expectRecessive && level != Recessive {
		r.formError = true
		r.immediatePending = true
	}
}

func bitsToInt(bits []BitLevel) int {
	v := 0
	for _, b := range bits {
		v = (v << 1) | int(b)
	}
	return v
}
