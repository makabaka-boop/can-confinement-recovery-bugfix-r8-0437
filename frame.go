package main

// BitLevel is the level seen on a CAN wire: dominant (0) wins wired-AND.
type BitLevel int

const (
	Dominant  BitLevel = 0
	Recessive BitLevel = 1
)

func (b BitLevel) String() string {
	if b == Dominant {
		return "0"
	}
	return "1"
}

// BitKind identifies the field or fixed-state role of a bit.
type BitKind string

const (
	KindSOF            BitKind = "SOF"
	KindID             BitKind = "ID"
	KindRTR            BitKind = "RTR"
	KindIDE            BitKind = "IDE"
	KindReserved       BitKind = "r0"
	KindDLC            BitKind = "DLC"
	KindData           BitKind = "DATA"
	KindCRC            BitKind = "CRC"
	KindStuff          BitKind = "STUFF"
	KindCRCDelim       BitKind = "CRC_DELIM"
	KindACKSlot        BitKind = "ACK_SLOT"
	KindACKDelim       BitKind = "ACK_DELIM"
	KindEOF            BitKind = "EOF"
	KindIntermission   BitKind = "INTERMISSION"
	KindErrorFlag      BitKind = "ERROR_FLAG"
	KindErrorDelimiter BitKind = "ERROR_DELIMITER"
	KindIdle           BitKind = "IDLE"
	KindCollision      BitKind = "COLLISION"
)

// Frame is a classical CAN 2.0A data frame. Remote frames are intentionally
// unsupported.
type Frame struct {
	ID      int
	Data    []byte
	Enqueue int // integer bit time supplied by the test
}

// EncodedBit is one physical bit. Raw is false for the inserted complement bit
// produced by CAN bit stuffing.
type EncodedBit struct {
	Level BitLevel
	Kind  BitKind
	Raw   bool
	Index int // raw index in SOF..CRC sequence; -1 outside the stuffed region
}

// EncodedFrame contains independently useful frame encoding, including the
// stuff region and the unstuffed trailing control bits.
type EncodedFrame struct {
	Frame      Frame
	Bits       []EncodedBit
	CRC        int
	StuffCount int
}

func infoBits(id int, data []byte) []BitLevel {
	dlc := len(data)
	raw := make([]BitLevel, 0, 19+8*dlc+15)
	raw = append(raw, Dominant) // SOF
	for i := 10; i >= 0; i-- {
		raw = append(raw, BitLevel((id>>i)&1))
	}
	raw = append(raw, Dominant, Dominant, Dominant) // RTR=0, IDE=0, r0=0
	for i := 3; i >= 0; i-- {
		raw = append(raw, BitLevel((dlc>>i)&1))
	}
	for _, b := range data {
		for i := 7; i >= 0; i-- {
			raw = append(raw, BitLevel((b>>uint(i))&1))
		}
	}
	return raw
}

// crc15 calculates the standard CAN CRC. Its register starts at zero and the
// 15 zero register bits are appended after the SOF..data input.
func crc15(info []BitLevel) int {
	const poly = 0x4599
	reg := 0
	for _, level := range info {
		next := (reg << 1) & 0x7fff
		// The feedback is XORed with polynomial taps whenever the bit shifted
		// out of the 15-stage register differs from the input bit.
		if ((reg >> 14) & 1) != int(level) {
			next ^= poly
		}
		reg = next
	}
	return reg
}

// EncodeDataFrame encodes SOF through EOF. Bits after CRC delimiter do not
// undergo bit stuffing; the ACK slot is encoded recessive as the sender would
// output before another node overwrites it.
func EncodeDataFrame(f Frame) EncodedFrame {
	if f.ID < 0 || f.ID > 0x7ff {
		panic("CAN 2.0A identifier must be 11 bits")
	}
	if len(f.Data) > 8 {
		panic("classical CAN data field must contain at most 8 bytes")
	}

	info := infoBits(f.ID, f.Data)
	crc := crc15(info)
	raw := append([]BitLevel(nil), info...)
	for i := 14; i >= 0; i-- {
		raw = append(raw, BitLevel((crc>>uint(i))&1))
	}

	// kindFor is only called for raw bits.
	kindFor := func(i int) BitKind {
		switch {
		case i < 1:
			return KindSOF
		case i < 12:
			return KindID
		case i == 12:
			return KindRTR
		case i == 13:
			return KindIDE
		case i == 14:
			return KindReserved
		case i < 19:
			return KindDLC
		case i < 19+len(f.Data)*8:
			return KindData
		default:
			return KindCRC
		}
	}

	bits := make([]EncodedBit, 0, len(raw)+24)
	run := 0
	stuffCount := 0
	for i, level := range raw {
		if i > 0 && level == raw[i-1] {
			run++
		} else {
			run = 1
		}
		bits = append(bits, EncodedBit{Level: level, Kind: kindFor(i), Raw: true, Index: i})
		// Stuff after five consecutive equal raw bits in SOF..CRC. The inserted
		// bit is not part of the raw run, so a six-bit source sequence produces
		// the source's sixth bit next.
		if run == 5 && i < len(raw) {
			s := Dominant
			if level == Dominant {
				s = Recessive
			}
			bits = append(bits, EncodedBit{Level: s, Kind: KindStuff, Raw: false, Index: -1})
			stuffCount++
			run = 0
		}
	}

	bits = append(bits, EncodedBit{Level: Recessive, Kind: KindCRCDelim, Raw: true, Index: -1})
	bits = append(bits, EncodedBit{Level: Recessive, Kind: KindACKSlot, Raw: true, Index: -1})
	bits = append(bits, EncodedBit{Level: Recessive, Kind: KindACKDelim, Raw: true, Index: -1})
	for i := 0; i < 7; i++ {
		bits = append(bits, EncodedBit{Level: Recessive, Kind: KindEOF, Raw: true, Index: -1})
	}
	return EncodedFrame{Frame: f, Bits: bits, CRC: crc, StuffCount: stuffCount}
}

// Equal reports whether two frames have the same identifier, data and enqueue
// time. It exists because Frame contains a slice and is not comparable with ==.
func (f Frame) Equal(other Frame) bool {
	if f.ID != other.ID || f.Enqueue != other.Enqueue || len(f.Data) != len(other.Data) {
		return false
	}
	for i := range f.Data {
		if f.Data[i] != other.Data[i] {
			return false
		}
	}
	return true
}

// BitString returns the physical 0/1 waveform, including stuff bits.
func (e EncodedFrame) BitString() string {
	out := make([]byte, len(e.Bits))
	for i, b := range e.Bits {
		out[i] = byte(b.Level.String()[0])
	}
	return string(out)
}
