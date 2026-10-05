package main

import "testing"

func confNode(t *testing.T, name string, tec int, recoverAt *int) *Node {
	t.Helper()
	n := NewNode(name, nil)
	c, err := NewConfinement(tec, recoverAt)
	if err != nil {
		t.Fatal(err)
	}
	n.Confinement = c
	return n
}

func stateOf(res *Result, name string) ConfinementState {
	return res.Confinement[name]
}

// Arbitration loss is not a transmission error: TEC must not move. Only the
// later successful completion decrements it by one.
func TestConfinementArbitrationExitDoesNotTouchTEC(t *testing.T) {
	a := confNode(t, "A", 200, nil) // higher ID loses
	b := NewNode("B", nil)          // lower ID wins and ACKs A's later frame
	a.Enqueue(Frame{ID: 0x200, Data: []byte{0x01}})
	b.Enqueue(Frame{ID: 0x100, Data: []byte{0x02}})

	res := runBus(t, []*Node{a, b}, nil)

	if got := stateOf(res, "A"); got.TEC != 199 || got.BusOff || got.Groups != 0 {
		t.Fatalf("arbitration exit changed confinement state: %+v", got)
	}
	if len(res.ArbitrationExits) != 1 || res.ArbitrationExits[0].Node != "A" {
		t.Fatalf("expected one arbitration exit by A: %+v", res.ArbitrationExits)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
}

// A real transmit failure (+8 per failed attempt) drives TEC to the 256
// saturation clamp; the unsent frame stays queued and the node is isolated.
func TestConfinementFailureSaturatesToBusOffAndKeepsQueue(t *testing.T) {
	lonely := confNode(t, "LONELY", 250, nil)
	f := Frame{ID: 0x123, Data: []byte{1, 2, 3}}
	lonely.Enqueue(f)

	bus, err := NewBus([]*Node{lonely}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := bus.RunConfinement(400)
	if err != nil {
		t.Fatal(err)
	}

	st := stateOf(res, "LONELY")
	if st.TEC != 256 || !st.BusOff || st.RecoveredAt != -1 {
		t.Fatalf("expected saturated bus-off, got %+v", st)
	}
	if res.Pending["LONELY"] != 1 {
		t.Fatalf("unsent frame must remain queued, pending=%d", res.Pending["LONELY"])
	}
	// The frame was never concluded either way: it is neither ok nor failed.
	for _, tr := range res.Transmissions {
		if tr.Node == "LONELY" {
			t.Fatalf("bus-off frame must not get a transmit conclusion: %+v", tr)
		}
	}
	// Once bus-off, the node must not drive anything on the wire — not even the
	// active error flag it would otherwise transmit. Locate the error flag bits.
	var flagBits []TraceBit
	for _, tb := range res.Trace {
		if tb.Kind == KindErrorFlag {
			flagBits = append(flagBits, tb)
		}
	}
	if len(flagBits) == 0 {
		t.Fatal("expected an error flag phase on the trace")
	}
	for _, tb := range flagBits {
		if _, drove := tb.Drivers["LONELY"]; drove {
			t.Fatalf("bus-off node drove at wire bit %d: %+v", tb.Time, tb)
		}
	}
}

// A node created already bus-off must not ACK, must not receive, and must not
// disturb a frame exchanged by healthy nodes.
func TestConfinementBusOffNodeCannotReceiveOrAck(t *testing.T) {
	tx := NewNode("TX", nil)
	rx := NewNode("RX", nil)
	bo := confNode(t, "BO", 256, nil)
	f := Frame{ID: 0x300, Data: []byte{0x77}}
	tx.Enqueue(f)

	res := runBus(t, []*Node{tx, rx, bo}, nil)

	if len(res.Errors) != 0 {
		t.Fatalf("healthy RX should have ACKed: %+v", res.Errors)
	}
	for _, r := range res.Received {
		if r.Node == "BO" {
			t.Fatalf("bus-off node produced a receive record: %+v", r)
		}
	}
	for _, tb := range res.Trace {
		if tb.Kind == KindACKSlot {
			if tb.Wire != Dominant {
				t.Fatalf("ACK slot not dominant: %+v", tb)
			}
			if _, drove := tb.Drivers["BO"]; drove {
				t.Fatalf("bus-off node drove the ACK slot: %+v", tb)
			}
			if got := tb.Drivers["RX"]; got != Dominant {
				t.Fatalf("healthy RX must be the ACK source: %+v", tb.Drivers)
			}
		}
		if _, drove := tb.Drivers["BO"]; drove {
			t.Fatalf("bus-off node drove at wire bit %d", tb.Time)
		}
	}
	if got := stateOf(res, "BO"); !got.BusOff || got.TEC != 256 {
		t.Fatalf("bus-off state changed without recovery: %+v", got)
	}
}

// Direct observer semantics: counting starts only after the explicit request;
// groups are non-overlapping; a dominant bit aborts only the partial group;
// recovery lands exactly on the 128th completed group.
func TestConfinementObserverGroupSemantics(t *testing.T) {
	// Without a request, even an unlimited idle run does nothing.
	idle, _ := NewConfinement(256, nil)
	for i := 0; i < 1408; i++ {
		idle.observe(Recessive, i)
	}
	if !idle.BusOff || idle.Groups != 0 || idle.RecessiveRun != 0 {
		t.Fatalf("recovery happened without request: %+v", idle)
	}

	c, _ := NewConfinement(256, nil)
	c.requestRecovery()

	feed := func(level BitLevel, n int) {
		for i := 0; i < n; i++ {
			c.observe(level, 0)
		}
	}
	feed(Recessive, 22) // exactly two non-overlapping groups
	if c.Groups != 2 || c.RecessiveRun != 0 {
		t.Fatalf("groups after 22 rec: %+v", c)
	}
	feed(Dominant, 1) // partial group (length 0) interrupted: banks survive
	if c.Groups != 2 || c.RecessiveRun != 0 {
		t.Fatalf("dominant wiped completed groups: %+v", c)
	}
	feed(Recessive, 3) // partial group of three
	feed(Dominant, 1)  // only this unfinished group is lost
	if c.Groups != 2 || c.RecessiveRun != 0 {
		t.Fatalf("dominant did not merely abort the partial group: %+v", c)
	}
	feed(Recessive, 10)
	if c.Groups != 2 || c.RecessiveRun != 10 {
		t.Fatalf("boundary of partial group wrong: %+v", c)
	}
	// Boundary: 127 groups banked is still bus-off; the 128th completing bit
	// performs the recovery on that exact bit.
	c2, _ := NewConfinement(256, nil)
	c2.requestRecovery()
	for i := 0; i < 127*11; i++ {
		c2.observe(Recessive, i)
	}
	if !c2.BusOff || c2.Groups != 127 {
		t.Fatalf("127 groups must not recover: %+v", c2)
	}
	c2.observe(Recessive, 127*11) // first recessive bit...
	for i := 1; i < 10; i++ {
		c2.observe(Recessive, 127*11+i)
	}
	if !c2.BusOff {
		t.Fatalf("recovered before the 11th bit of group 128: %+v", c2)
	}
	at := 128*11 - 1
	c2.observe(Recessive, at)
	if c2.BusOff || c2.TEC != 0 || c2.RecoveredAt != at || c2.RecoveryEnabled {
		t.Fatalf("recovery boundary wrong: %+v", c2)
	}
}

// scanRecoveryBit independently re-derives the recovery instant from the
// physical trace: non-overlapping groups of eleven recessive wire bits,
// completed groups surviving any dominant bit.
func scanRecoveryBit(trace []TraceBit) int {
	groups, run := 0, 0
	for _, tb := range trace {
		if tb.Wire == Dominant {
			run = 0
			continue
		}
		run++
		if run == 11 {
			groups++
			run = 0
		}
		if groups == 128 {
			return tb.Time
		}
	}
	return -1
}

// End-to-end wire evidence: a dominant frame occurs after two groups have
// already been banked. Those groups survive the interruption, recovery lands
// on exactly the bit an independent trace scan derives, and the queued frame
// is then transmitted from attempt 1.
func TestConfinementDominantInterruptKeepsBankedGroupsAndQueueResumes(t *testing.T) {
	recoverAt := 0
	tx := confNode(t, "TX", 256, &recoverAt)
	other := NewNode("OTHER", nil)
	rx := NewNode("RX", nil) // ACK source for every frame on the bus
	txFrame := Frame{ID: 0x201, Data: []byte{0xab}}
	otherFrame := Frame{ID: 0x102, Data: nil, Enqueue: 25}
	tx.Enqueue(txFrame)
	other.Enqueue(otherFrame)

	bus, err := NewBus([]*Node{tx, other, rx}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := bus.RunConfinement(3000)
	if err != nil {
		t.Fatal(err)
	}

	st := stateOf(res, "TX")
	if st.BusOff || st.Groups != 128 || st.TEC != 0 {
		t.Fatalf("TX did not complete recovery: %+v", st)
	}
	want := scanRecoveryBit(res.Trace)
	if want < 0 {
		t.Fatal("independent trace scan never saw 128 groups")
	}
	if st.RecoveredAt != want {
		t.Fatalf("recovery at %d, trace scan says %d", st.RecoveredAt, want)
	}
	// The very next wire bit starts the retained queued frame.
	next := res.Trace[want+1]
	if next.Kind != KindSOF || next.Wire != Dominant {
		t.Fatalf("queue did not resume immediately after recovery: %+v", next)
	}
	if res.Pending["TX"] != 0 {
		t.Fatalf("recovered queue not drained: pending=%d", res.Pending["TX"])
	}
	var txRec *TransmitRecord
	for i := range res.Transmissions {
		if res.Transmissions[i].Node == "TX" {
			txRec = &res.Transmissions[i]
		}
	}
	if txRec == nil || txRec.Status != "ok" || txRec.Attempts != 1 {
		t.Fatalf("recovered frame result wrong: %+v", txRec)
	}
	// Bit-accurate waveform of the recovered transmission, ACK slot included.
	enc := EncodeDataFrame(txFrame)
	for j, b := range enc.Bits {
		tb := res.Trace[want+1+j]
		level := b.Level
		if b.Kind == KindACKSlot {
			level = Dominant
		}
		if tb.Wire != level {
			t.Fatalf("resumed frame mismatch at offset %d (wire bit %d): got %d want %d",
				j, tb.Time, tb.Wire, level)
		}
	}
}

// Observation deadline ends with the partially recovered state and the still
// queued frames as evidence; nothing is sent while blocked.
func TestConfinementDeadlineEvidence(t *testing.T) {
	recoverAt := 0
	tx := confNode(t, "TX", 256, &recoverAt)
	tx.Enqueue(Frame{ID: 0x333, Data: []byte{1}})
	tx.Enqueue(Frame{ID: 0x334, Data: []byte{2}})

	bus, err := NewBus([]*Node{tx}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := bus.RunConfinement(1000) // 90 complete groups, partial run of 10
	if err != nil {
		t.Fatal(err)
	}
	st := stateOf(res, "TX")
	if !st.BusOff || st.Groups != 90 || st.RecessiveRun != 10 {
		t.Fatalf("deadline state wrong: %+v", st)
	}
	if res.Pending["TX"] != 2 {
		t.Fatalf("queue must survive the deadline, pending=%d", res.Pending["TX"])
	}
	if len(res.Transmissions) != 0 {
		t.Fatalf("blocked node must not transmit: %+v", res.Transmissions)
	}
	for _, tb := range res.Trace {
		if tb.Kind != KindIdle {
			t.Fatalf("wire not idle while blocked at bit %d: %s", tb.Time, tb.Kind)
		}
	}
}

// A successful completion decrements TEC by one, clamped at zero.
func TestConfinementSuccessDecrementsTEC(t *testing.T) {
	tx := confNode(t, "TX", 5, nil)
	rx := NewNode("RX", nil)
	tx.Enqueue(Frame{ID: 0x400, Data: nil})
	res := runBus(t, []*Node{tx, rx}, nil)
	if got := stateOf(res, "TX"); got.TEC != 4 || got.BusOff {
		t.Fatalf("success should decrement TEC: %+v", got)
	}
}
