package main

import (
	"math/big"
	"reflect"
	"testing"
)

// referenceCRC15 independently computes the GF(2) polynomial remainder using
// math/big integers rather than the production shift register.
func referenceCRC15(info []BitLevel) int {
	var dividend big.Int
	for _, level := range info {
		dividend.Lsh(&dividend, 1)
		if level == Recessive {
			dividend.SetBit(&dividend, 0, 1)
		}
	}
	dividend.Lsh(&dividend, 15) // append 15 zero register bits

	generator := big.NewInt(0xC599) // x^15 + production CRC taps
	for pos := len(info) + 14; pos >= 15; pos-- {
		if dividend.Bit(pos) == 1 {
			var shifted big.Int
			shifted.Lsh(generator, uint(pos-15))
			dividend.Xor(&dividend, &shifted)
		}
	}
	return int(dividend.Int64()) & 0x7fff
}

// referenceEncode is an independent, straightforward encoder used by tests to
// check the production frame layout and CRC together.
func referenceEncode(t *testing.T, f Frame) []EncodedBit {
	t.Helper()
	raw := infoBits(f.ID, f.Data)
	crc := referenceCRC15(raw)
	for i := 14; i >= 0; i-- {
		raw = append(raw, BitLevel((crc>>uint(i))&1))
	}
	var out []EncodedBit
	run := 0
	for i, level := range raw {
		kind := rawKindFor(f, i)
		out = append(out, EncodedBit{Level: level, Kind: kind, Raw: true, Index: i})
		if i > 0 && level == raw[i-1] {
			run++
		} else {
			run = 1
		}
		if run == 5 {
			out = append(out, EncodedBit{Level: BitLevel(1 - int(level)), Kind: KindStuff, Raw: false, Index: -1})
			run = 0
		}
	}
	out = append(out, EncodedBit{Level: Recessive, Kind: KindCRCDelim, Raw: true, Index: -1})
	out = append(out, EncodedBit{Level: Recessive, Kind: KindACKSlot, Raw: true, Index: -1})
	out = append(out, EncodedBit{Level: Recessive, Kind: KindACKDelim, Raw: true, Index: -1})
	for i := 0; i < 7; i++ {
		out = append(out, EncodedBit{Level: Recessive, Kind: KindEOF, Raw: true, Index: -1})
	}
	return out
}

func rawKindFor(f Frame, i int) BitKind {
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
	case i < 19+8*len(f.Data):
		return KindData
	default:
		return KindCRC
	}
}

func assertEncodedEqual(t *testing.T, f Frame) EncodedFrame {
	t.Helper()
	got := EncodeDataFrame(f)
	want := referenceEncode(t, f)
	if !reflect.DeepEqual(got.Bits, want) {
		for i := range want {
			if got.Bits[i] != want[i] {
				t.Fatalf("first encoding mismatch at bit %d for ID=%#x: got=%+v want=%+v",
					i, f.ID, got.Bits[i], want[i])
			}
		}
		t.Fatalf("encoding length mismatch for ID=%#x data=% X", f.ID, f.Data)
	}
	if got.CRC != referenceCRC15(infoBits(f.ID, f.Data)) {
		t.Fatalf("CRC mismatch for ID=%#x", f.ID)
	}
	r := newReceiver()
	for _, b := range got.Bits {
		r.feed(b.Level, b.Kind)
	}
	if !r.delivered() || r.nextBitError != "" || r.crcError || r.formError {
		t.Fatalf("reference receiver rejected ID=%#x", f.ID)
	}
	if !r.frame.Equal(f) {
		t.Fatalf("decoded frame=%#v want=%#v", r.frame, f)
	}
	return got
}

func TestIndependentEncodingCRCAndDestuff(t *testing.T) {
	frames := []Frame{
		{ID: 0x000, Data: make([]byte, 8)},
		{ID: 0x7ff, Data: []byte{0xff, 0xff, 0x00, 0xaa}},
		{ID: 0x456, Data: []byte{0xaa}},
		{ID: 0x234, Data: []byte{0x00}},
	}
	for _, f := range frames {
		e := assertEncodedEqual(t, f)
		if e.StuffCount < 1 {
			t.Fatalf("expected at least one stuff bit for %#x", f.ID)
		}
	}
}

func TestStuffBoundariesAcrossFieldsAndCRC(t *testing.T) {
	// SOF + ID 0 + RTR/IDE/r0 create a long run of zeroes that crosses SOF/ID
	// and ID/control; later runs cross data/CRC boundaries.
	f := Frame{ID: 0x000, Data: make([]byte, 8)}
	e := EncodeDataFrame(f)
	var positions []int
	for i, b := range e.Bits {
		if b.Kind == KindStuff {
			positions = append(positions, i)
		}
	}
	want := []int{5, 11, 17, 24, 30, 36, 42, 48, 54, 60, 66, 72, 78, 84, 90, 96}
	if !reflect.DeepEqual(positions, want) {
		t.Fatalf("stuff positions=%v want=%v", positions, want)
	}
	r := newReceiver()
	for _, b := range e.Bits {
		r.feed(b.Level, b.Kind)
	}
	if !r.delivered() {
		t.Fatalf("destuff/CRC failed at boundary frame")
	}
}

func runBus(t *testing.T, nodes []*Node, faults []ReceiverBitFault) *Result {
	t.Helper()
	bus, err := NewBus(nodes, faults)
	if err != nil {
		t.Fatal(err)
	}
	res, err := bus.Run()
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestSimultaneousTransmissionAndArbitration(t *testing.T) {
	nodes := []*Node{
		NewNode("N1", nil),
		NewNode("N2", nil),
		NewNode("N3", nil),
	}
	f1 := Frame{ID: 0x540, Data: []byte{0x10, 0x20}}
	f2 := Frame{ID: 0x480, Data: []byte{0x30}}
	f3 := Frame{ID: 0x440, Data: []byte{0x40, 0x01}}
	nodes[0].Enqueue(f1)
	nodes[1].Enqueue(f2)
	nodes[2].Enqueue(f3)

	res := runBus(t, nodes, nil)
	// Frame 1: N3 (0x440) wins; N1 and N2 both lose bit by bit and keep
	// receiving. Frame 2 after intermission: N2 (0x480) wins, so N1 loses a
	// second time. Frame 3: N1 transmits alone. No identity-based ordering.
	firstFrameLen := len(EncodeDataFrame(f3).Bits)
	recovery := 3 // only the three intermission bits follow a successful EOF
	secondStart := firstFrameLen + recovery

	wantExits := []struct {
		node  string
		wire  int
		field string
		idBit int
	}{
		{"N1", 3, "ID", 3},
		{"N2", 4, "ID", 4},
		{"N1", secondStart + 3, "ID", 3},
	}
	if len(res.ArbitrationExits) != len(wantExits) {
		t.Fatalf("exits=%+v", res.ArbitrationExits)
	}
	for i, want := range wantExits {
		got := res.ArbitrationExits[i]
		if got.Node != want.node || got.WireBit != want.wire ||
			got.Field != want.field || got.BitNumber != want.idBit {
			t.Fatalf("exit %d=%+v want=%+v", i, got, want)
		}
	}
	winner := EncodeDataFrame(f3)
	for i := 0; i < len(winner.Bits); i++ {
		if winner.Bits[i].Kind == KindACKSlot {
			continue // driven dominant on the bus by correctly receiving nodes
		}
		if res.Trace[i].Wire != winner.Bits[i].Level {
			t.Fatalf("winner trace mismatch at %d: %d want %d", i, res.Trace[i].Wire, winner.Bits[i].Level)
		}
	}
	ackSlot := len(winner.Bits) - 9
	if res.Trace[ackSlot].Kind != KindACKSlot || res.Trace[ackSlot].Wire != Dominant {
		t.Fatalf("ACK slot was not dominant: %+v", res.Trace[ackSlot])
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors %+v", res.Errors)
	}
	if len(res.Transmissions) != 3 {
		t.Fatalf("transmissions=%+v", res.Transmissions)
	}
	for _, tr := range res.Transmissions {
		if tr.Status != "ok" {
			t.Fatalf("unexpected transmit failure %+v", tr)
		}
	}
}

func TestFilterDoesNotBlockACK(t *testing.T) {
	tx := NewNode("TX", nil)
	rx := NewNode("RX", func(f Frame) bool { return false })
	f := Frame{ID: 0x123, Data: []byte{1, 2, 3}}
	tx.Enqueue(f)
	res := runBus(t, []*Node{tx, rx}, nil)
	if len(res.Errors) != 0 {
		t.Fatalf("filtered receiver failed to ACK: %+v", res.Errors)
	}
	if len(res.Received) != 1 || res.Received[0].Node != "RX" || res.Received[0].Accepted {
		t.Fatalf("receive records=%+v", res.Received)
	}
}

func TestMissingACKRetriesThreeTimes(t *testing.T) {
	tx := NewNode("LONELY", nil)
	f := Frame{ID: 0x123, Data: []byte{1, 2, 3}}
	tx.Enqueue(f)
	res := runBus(t, []*Node{tx}, nil)
	if len(res.Transmissions) != 1 {
		t.Fatalf("transmissions=%+v", res.Transmissions)
	}
	tr := res.Transmissions[0]
	if tr.Status != "failed" || tr.Attempts != 3 || tr.Error != reasonACK {
		t.Fatalf("unexpected result %+v", tr)
	}
	if len(res.Errors) != 3 {
		t.Fatalf("errors=%+v", res.Errors)
	}
	for _, e := range res.Errors {
		if e.Reason != reasonACK {
			t.Fatalf("unexpected reason %q", e.Reason)
		}
	}
}

func wirePositionOf(e EncodedFrame, rawIndex int) int {
	for i, b := range e.Bits {
		if b.Raw && b.Index == rawIndex {
			return i
		}
	}
	panic("raw index not found")
}

func TestSameIdentifierDifferentDataIsBitError(t *testing.T) {
	a := NewNode("A", nil)
	bnode := NewNode("B", nil)
	rx := NewNode("RX", nil)
	fa := Frame{ID: 0x234, Data: []byte{0xff}}
	fb := Frame{ID: 0x234, Data: []byte{0x00}}
	a.Enqueue(fa)
	bnode.Enqueue(fb)
	res := runBus(t, []*Node{a, bnode, rx}, nil)

	if len(res.ArbitrationExits) != 0 {
		t.Fatalf("same-ID collision must not be arbitration: %+v", res.ArbitrationExits)
	}
	if len(res.Errors) != 3 {
		t.Fatalf("errors=%+v", res.Errors)
	}
	e := res.Errors[0]
	if e.Reason != reasonBit {
		t.Fatalf("reason=%s", e.Reason)
	}
	expected := EncodeDataFrame(fa)
	wantBit := wirePositionOf(expected, 19) // first data byte MSB
	if e.WireBit != wantBit {
		t.Fatalf("error bit=%d want=%d", e.WireBit, wantBit)
	}
	if res.Trace[e.WireBit].Wire != Dominant {
		t.Fatalf("collision wire level was recessive")
	}
	var failed int
	for _, tr := range res.Transmissions {
		if tr.Status == "failed" && tr.Attempts == 3 && tr.Error == reasonBit {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("both same-ID senders must fail: %+v", res.Transmissions)
	}
}

func TestReceiverBitFaultSpecifiesNodeAndWirePosition(t *testing.T) {
	tx := NewNode("TX", nil)
	rx := NewNode("RX", nil)
	goodRX := NewNode("GOOD_RX", nil)
	f := Frame{ID: 0x456, Data: []byte{0xaa}}
	tx.Enqueue(f)
	e := EncodeDataFrame(f)
	// Raw index 19 is the data byte's MSB (0xaa bit = recessive); forcing it
	// dominant corrupts the data only at the named receiver and fails its CRC.
	fault := ReceiverBitFault{ReceiverNode: "RX", AtWireBit: wirePositionOf(e, 19), Level: Dominant}
	res := runBus(t, []*Node{tx, rx, goodRX}, []ReceiverBitFault{fault})
	if len(res.Errors) != 1 || res.Errors[0].Reason != reasonCRC {
		t.Fatalf("errors=%+v", res.Errors)
	}
	if res.Errors[0].FlagWireBit != res.Errors[0].WireBit {
		t.Fatalf("CRC flag should occur in ACK delimiter: %+v", res.Errors[0])
	}
	if len(res.Transmissions) != 1 || res.Transmissions[0].Status != "ok" ||
		res.Transmissions[0].Attempts != 2 {
		t.Fatalf("second attempt should succeed: %+v", res.Transmissions)
	}
	tb := res.Trace[fault.AtWireBit]
	if got := tb.FaultSamples["RX"]; got != Dominant {
		t.Fatalf("fault sample not recorded: %+v", tb)
	}
	if tb.Wire != Recessive {
		t.Fatalf("receiver-local fault must not alter shared wire level")
	}
}

func TestDelayedEnqueueStartsAtGivenBitTime(t *testing.T) {
	a := NewNode("A", nil)
	rx := NewNode("RX", nil)
	f := Frame{ID: 0x100, Data: nil, Enqueue: 3}
	a.Enqueue(f)
	res := runBus(t, []*Node{a, rx}, nil)

	// Three recessive idle bits (times 0,1,2), SOF at time 3.
	for i := 0; i < 3; i++ {
		if res.Trace[i].Kind != KindIdle || res.Trace[i].Wire != Recessive {
			t.Fatalf("expected idle at %d, got %+v", i, res.Trace[i])
		}
	}
	if res.Trace[3].Kind != KindSOF || res.Trace[3].Wire != Dominant {
		t.Fatalf("frame did not begin at enqueue bit time 3: %+v", res.Trace[3])
	}
}

func TestArbitrationLoserKeepsReceivingAndACKs(t *testing.T) {
	hi := NewNode("HI", nil) // higher ID loses
	lo := NewNode("LO", nil) // lower ID wins
	// No separate receiver: the loser must still ACK the winner's frame.
	fhi := Frame{ID: 0x200, Data: []byte{0x01}}
	flo := Frame{ID: 0x100, Data: []byte{0x02}}
	hi.Enqueue(fhi)
	lo.Enqueue(flo)
	res := runBus(t, []*Node{hi, lo}, nil)

	if len(res.Errors) != 0 {
		t.Fatalf("loser must keep receiving and ACK: %+v", res.Errors)
	}
	// Check the first ACK slot: that belongs to the winner LO and must be driven
	// dominant by the arbitration loser HI, which kept receiving.
	for i := range res.Trace {
		if res.Trace[i].Kind == KindACKSlot {
			ack := res.Trace[i]
			if ack.Wire != Dominant || ack.Drivers["HI"] != Dominant {
				t.Fatalf("arbitration loser did not drive first ACK: %v", ack.Drivers)
			}
			break
		}
	}
}

func TestInjectedBitErrorCausesOneRetryThenSuccess(t *testing.T) {
	tx := NewNode("TX", nil)
	rx := NewNode("RX", nil)
	f := Frame{ID: 0x333, Data: []byte{0x55}}
	tx.Enqueue(f)
	e := EncodeDataFrame(f)
	// Force a recessive data bit dominant only at the sender's sample; this is
	// an injected TX bit error at the named physical wire position.
	target := wirePositionOf(e, 20) // a data bit that is recessive for 0x55
	if e.Bits[target].Level != Recessive {
		t.Fatalf("test setup: target bit not recessive")
	}
	// The fault targets the transmitter itself, so its own sample contradicts
	// what it sent and it raises a bit error.
	fault := ReceiverBitFault{ReceiverNode: "TX", AtWireBit: target, Level: Dominant}
	res := runBus(t, []*Node{tx, rx}, []ReceiverBitFault{fault})

	if len(res.Errors) != 1 || res.Errors[0].Reason != reasonBit {
		t.Fatalf("errors=%+v", res.Errors)
	}
	if res.Errors[0].WireBit != target {
		t.Fatalf("bit error at %d want %d", res.Errors[0].WireBit, target)
	}
	if len(res.Transmissions) != 1 || res.Transmissions[0].Status != "ok" ||
		res.Transmissions[0].Attempts != 2 {
		t.Fatalf("frame should succeed on retry: %+v", res.Transmissions)
	}
	// Retry waveform must contain a clean second copy of the frame.
	var cleanFrames int
	for i := 0; i+len(e.Bits) <= len(res.Trace); i++ {
		match := true
		for j := range e.Bits {
			tb := res.Trace[i+j]
			want := e.Bits[j].Level
			if e.Bits[j].Kind == KindACKSlot {
				want = Dominant
			}
			if tb.Wire != want {
				match = false
				break
			}
		}
		if match {
			cleanFrames++
		}
	}
	if cleanFrames != 1 {
		t.Fatalf("expected exactly one clean frame copy, found %d", cleanFrames)
	}
}

func TestStuffBoundaryFollowedByData(t *testing.T) {
	// 0x00 data has long zero runs; a 0xff byte immediately after forces the
	// run/reset logic across a stuff boundary inside the data field.
	f := Frame{ID: 0x001, Data: []byte{0x00, 0xff}}
	e := assertEncodedEqual(t, f)
	if e.StuffCount == 0 {
		t.Fatalf("expected stuffing")
	}
}
