package main

import "testing"

func ptr(v int) *int { return &v }

// replayRecovery independently re-walks the recorded physical wire trace and
// applies the teaching model exactly: counting starts at the explicit recovery
// request, groups are non-overlapping runs of 11 recessive bits, a dominant bit
// discards only the unfinished group, and counting stops once 128 groups close.
// It returns the number of complete groups, the partial run at trace end (or at
// recovery), and the wire bit at which the 128th group closed.
func replayRecovery(trace []TraceBit, requestAt int) (groups, run, recoveredAt int) {
	recoveredAt = -1
	counting := false
	for _, bit := range trace {
		if bit.Time == requestAt {
			counting = true
		}
		if !counting || recoveredAt >= 0 {
			continue
		}
		if bit.Wire == Dominant {
			run = 0 // only the unfinished group is lost
			continue
		}
		run++
		if run == 11 {
			groups++
			run = 0
			if groups == 128 {
				recoveredAt = bit.Time
			}
		}
	}
	return
}

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

// A real transmission failure increments the TEC, saturates it at 256 and puts
// the node bus-off; arbitration loss does neither.
func TestConfinementRealFailureSaturatesAndArbitrationDoesNot(t *testing.T) {
	loser := confNode(t, "HI", 248, nil)
	winner := NewNode("LO", nil)
	rx := NewNode("RX", nil)
	// 0x200 vs 0x100: HI loses arbitration on ID bit 2, keeps the frame.
	loser.Enqueue(Frame{ID: 0x200, Data: []byte{0x01}})
	winner.Enqueue(Frame{ID: 0x100, Data: []byte{0x02}})
	res := runBus(t, []*Node{loser, winner, rx}, nil)

	if len(res.ArbitrationExits) != 1 || res.ArbitrationExits[0].Node != "HI" {
		t.Fatalf("expected one arbitration exit: %+v", res.ArbitrationExits)
	}
	got := res.Confinement["HI"]
	// Arbitration exit is not a failure: TEC untouched by the exit, still
	// active, no error events, and the queued frame later succeeds on its own
	// (that success performs the ordinary -1).
	if got.TEC != 247 || got.BusOff || got.RecoveryEnabled {
		t.Fatalf("arbitration exit affected the TEC (expected only a later success -1): %+v", got)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("arbitration produced error events: %+v", res.Errors)
	}
	if res.Pending["HI"] != 0 {
		t.Fatalf("loser frame did not complete later, pending=%d", res.Pending["HI"])
	}
	var hiOK bool
	for _, tr := range res.Transmissions {
		if tr.Node == "HI" && tr.Status == "ok" {
			hiOK = true
		}
	}
	if !hiOK {
		t.Fatalf("lost frame was not transmitted successfully afterwards: %+v", res.Transmissions)
	}
}

// Repeated real failures pin TEC at 256 and the unsent frame stays in the
// queue; the limit-end state and queue evidence must agree.
func TestConfinementSaturationKeepsPendingFrame(t *testing.T) {
	sender := confNode(t, "LONELY", 240, nil)
	f := Frame{ID: 0x123, Data: []byte{1, 2, 3}}
	sender.Enqueue(f)
	res := runBus(t, []*Node{sender}, nil) // three missing-ACK attempts

	got := res.Confinement["LONELY"]
	if !got.BusOff || got.TEC != 256 {
		t.Fatalf("expected bus-off TEC=256, got %+v", got)
	}
	if got.RecoveredAt != -1 || got.RecoveryEnabled || got.Groups != 0 {
		t.Fatalf("bus-off must not auto-recover: %+v", got)
	}
	if res.Pending["LONELY"] != 1 {
		t.Fatalf("unsent frame must remain queued, pending=%d", res.Pending["LONELY"])
	}
}

// A bus-off node must not ACK: the only other sender sees three missing ACKs.
func TestConfinementBlockedNodeDoesNotAckOrReceive(t *testing.T) {
	blocked := confNode(t, "OFF", 256, nil)
	sender := NewNode("TX", nil)
	blocked.Enqueue(Frame{ID: 0x321, Data: []byte{9}}) // never starts
	sender.Enqueue(Frame{ID: 0x123, Data: []byte{1, 2, 3}})
	res := runBus(t, []*Node{blocked, sender}, nil)

	if len(res.Errors) != 3 {
		t.Fatalf("expected three missing-ACK events, got %+v", res.Errors)
	}
	for _, e := range res.Errors {
		if e.Reason != reasonACK {
			t.Fatalf("unexpected reason %q", e.Reason)
		}
	}
	for _, r := range res.Received {
		if r.Node == "OFF" {
			t.Fatalf("bus-off node received a frame: %+v", r)
		}
	}
	for i, bit := range res.Trace {
		if lvl, ok := bit.Drivers["OFF"]; ok && lvl == Dominant {
			switch bit.Kind {
			case KindACKSlot:
				t.Fatalf("bus-off node drove ACK at bit %d", i)
			case KindErrorFlag:
				t.Fatalf("bus-off node drove the error flag at bit %d", i)
			default:
				t.Fatalf("bus-off node drove the wire dominant at bit %d (%s)", i, bit.Kind)
			}
		}
	}
	if res.Pending["OFF"] != 1 {
		t.Fatalf("blocked node lost its queue: pending=%d", res.Pending["OFF"])
	}
}

// Recovery needs an explicit request; plenty of recessive wire without one must
// not bring the node back, and its queue must survive.
func TestConfinementNoRecoveryWithoutRequest(t *testing.T) {
	off := confNode(t, "OFF", 256, nil) // no RecoverAt
	talker := NewNode("T", nil)
	rx := NewNode("RX", nil)
	off.Enqueue(Frame{ID: 0x321, Data: []byte{9}})
	// Frame late enough that the bus sees well over 128*11 recessive idle bits.
	talker.Enqueue(Frame{ID: 0x100, Data: []byte{1}, Enqueue: 1900})
	bus, err := NewBus([]*Node{off, talker, rx}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := bus.RunConfinement(2000)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Confinement["OFF"]
	if !got.BusOff || got.TEC != 256 || got.RecoveryEnabled {
		t.Fatalf("node recovered without a request: %+v", got)
	}
	if got.Groups != 0 || got.RecessiveRun != 0 || got.RecoveredAt != -1 {
		t.Fatalf("observation ran without a request: %+v", got)
	}
	if res.Pending["OFF"] != 1 {
		t.Fatalf("queue must survive bus-off, pending=%d", res.Pending["OFF"])
	}
}

// Recovery boundary, counted straight off the physical trace in pure idle:
// bits before the request never count, the 1408th counted recessive bit closes
// the 128th non-overlapping group and the node returns.
func TestConfinementRecoveryBoundaryOnTrace(t *testing.T) {
	const requestAt = 10
	off := confNode(t, "OFF", 256, ptr(requestAt))
	bus, err := NewBus([]*Node{off}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := bus.RunConfinement(1450)
	if err != nil {
		t.Fatal(err)
	}
	groups, run, replayAt := replayRecovery(res.Trace, requestAt)
	if replayAt < 0 {
		t.Fatalf("trace replay never saw 128 groups (groups=%d run=%d)", groups, run)
	}
	got := res.Confinement["OFF"]
	if got.BusOff || got.TEC != 0 {
		t.Fatalf("node did not leave bus-off: %+v", got)
	}
	if got.RecoveredAt != replayAt {
		t.Fatalf("recovered at %d but trace replay says %d", got.RecoveredAt, replayAt)
	}
	if want := requestAt + 128*11 - 1; replayAt != want {
		t.Fatalf("recovery bit=%d want=%d", replayAt, want)
	}
	rb := res.Trace[replayAt]
	if rb.Wire != Recessive || rb.Kind != KindIdle {
		t.Fatalf("recovery bit was not a recessive idle bit: %+v", rb)
	}
	if res.Trace[requestAt-1].Wire != Recessive {
		t.Fatalf("test setup: pre-request bit not recessive")
	}
}

// End-to-end, driven through a real fault: one genuine transmission failure
// moves TEC 248 -> 256; after the explicit request the node counts only off the
// actual wire (frames interrupt groups with dominant bits), and the retained
// queue head is transmitted successfully once 128 groups have passed.
func TestConfinementFailureThenObservedRecoveryResumesQueue(t *testing.T) {
	const requestAt = 30 // after the bit error at 20 puts A bus-off
	sender := confNode(t, "A", 248, ptr(requestAt))
	rx := NewNode("RX", nil)
	f := Frame{ID: 0x456, Data: []byte{0xaa}}
	sender.Enqueue(f)
	e := EncodeDataFrame(f)
	target := wirePositionOf(e, 19) // recessive data bit forced dominant at A
	fault := ReceiverBitFault{ReceiverNode: "A", AtWireBit: target, Level: Dominant}
	bus, err := NewBus([]*Node{sender, rx}, []ReceiverBitFault{fault})
	if err != nil {
		t.Fatal(err)
	}
	res, err := bus.RunConfinement(2000)
	if err != nil {
		t.Fatal(err)
	}

	got := res.Confinement["A"]
	if got.TEC != 0 || got.BusOff || got.RecoveryEnabled {
		t.Fatalf("unexpected final state: %+v", got)
	}
	if got.RecoveredAt < 0 {
		t.Fatalf("node never recovered")
	}
	groups, run, replayAt := replayRecovery(res.Trace, requestAt)
	if replayAt != got.RecoveredAt || groups != 128 || run != 0 {
		t.Fatalf("state recovery=%d disagrees with trace replay: groups=%d run=%d at=%d",
			got.RecoveredAt, groups, run, replayAt)
	}
	// The fault bit is inside the first failed attempt; the error flag that
	// follows is dominant and must interrupt only the unfinished group. The
	// recovered retry starts strictly after the recovery bit on the real wire.
	if res.Trace[target].FaultSamples["A"] != Dominant {
		t.Fatalf("fault missing from trace: %+v", res.Trace[target])
	}
	if res.Trace[got.RecoveredAt].Wire != Recessive {
		t.Fatalf("recovery must close on a recessive wire bit")
	}
	var nextSOF int
	for i := got.RecoveredAt + 1; i < len(res.Trace); i++ {
		if res.Trace[i].Kind == KindSOF {
			nextSOF = i
			break
		}
	}
	if nextSOF == 0 {
		t.Fatalf("retained frame did not restart after recovery")
	}
	if res.Pending["A"] != 0 {
		t.Fatalf("original queue did not drain after recovery, pending=%d", res.Pending["A"])
	}
	var okRecord *TransmitRecord
	for i := range res.Transmissions {
		if res.Transmissions[i].Node == "A" {
			okRecord = &res.Transmissions[i]
		}
	}
	if okRecord == nil || okRecord.Status != "ok" || !okRecord.Frame.Equal(f) {
		t.Fatalf("retained frame result: %+v", okRecord)
	}
	// While bus-off the node never received anything.
	for _, r := range res.Received {
		if r.Node == "A" {
			t.Fatalf("bus-off node received a frame: %+v", r)
		}
	}
}

// Dominant bits during observation interrupt only the unfinished group;
// completed groups survive and the state at the limit matches the wire trace.
func TestConfinementDominantInterruptKeepsCompletedGroups(t *testing.T) {
	// A fails its own first attempt with an injected bit error and goes bus-off;
	// after the explicit request, B sends two clean frames whose dominant
	// sections repeatedly interrupt unfinished groups while completed groups
	// must be retained.
	const requestAt = 60
	a := confNode(t, "A", 248, ptr(requestAt))
	b := NewNode("B", nil)
	rx := NewNode("RX", nil)
	fa := Frame{ID: 0x456, Data: []byte{0xaa}}
	fb := Frame{ID: 0x100, Data: []byte{0x02}, Enqueue: 200}
	a.Enqueue(fa)
	b.Enqueue(fb)
	b.Enqueue(Frame{ID: 0x101, Data: []byte{0x03}, Enqueue: 400})
	ea := EncodeDataFrame(fa)
	fault := ReceiverBitFault{ReceiverNode: "A", AtWireBit: wirePositionOf(ea, 19), Level: Dominant}
	bus, err := NewBus([]*Node{a, b, rx}, []ReceiverBitFault{fault})
	if err != nil {
		t.Fatal(err)
	}
	res, err := bus.RunConfinement(500)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Confinement["A"]
	if !got.BusOff {
		t.Fatalf("A should still be bus-off at limit, got %+v", got)
	}
	groups, run, replayAt := replayRecovery(res.Trace, requestAt)
	if replayAt >= 0 {
		t.Fatalf("recovery unexpectedly completed inside the limit")
	}
	if got.Groups != groups || got.RecessiveRun != run {
		t.Fatalf("state groups=%d run=%d disagrees with wire replay groups=%d run=%d",
			got.Groups, got.RecessiveRun, groups, run)
	}
	// A enters bus-off around bit 50 while counting from bit 0; the first B
	// frame starts after enough recessive idle to bank groups. Under the old
	// "a dominant bit clears all groups" rule these banked groups would be lost.
	if groups < 1 {
		t.Fatalf("expected banked groups before the dominant traffic, got %d", groups)
	}
	// Dominant content actually occurred during observation.
	var dominantDuringObservation bool
	for _, bit := range res.Trace {
		if bit.Time >= requestAt && bit.Wire == Dominant && bit.Kind != KindErrorFlag {
			dominantDuringObservation = true
		}
	}
	if !dominantDuringObservation {
		t.Fatal("test setup: no dominant traffic observed")
	}
	if res.Pending["A"] != 1 {
		t.Fatalf("unsent frame must stay queued, pending=%d", res.Pending["A"])
	}
}

// Direct unit checks of the observation state machine.
func TestConfinementObservationStateMachine(t *testing.T) {
	c, err := NewConfinement(256, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Without an explicit request, thousands of recessive bits do nothing.
	for i := 0; i < 5000; i++ {
		c.observe(Recessive, i)
	}
	if !c.BusOff || c.Groups != 0 || c.RecessiveRun != 0 {
		t.Fatalf("observation ran before the request: %+v", c)
	}
	c.requestRecovery()
	for i := 0; i < 10; i++ {
		c.observe(Recessive, i)
	}
	if c.Groups != 0 || c.RecessiveRun != 10 {
		t.Fatalf("partial group state wrong: %+v", c)
	}
	c.observe(Dominant, 10) // interrupts the unfinished 10-bit group only
	if c.Groups != 0 || c.RecessiveRun != 0 {
		t.Fatalf("dominant did not reset the unfinished group: %+v", c)
	}
	for i := 0; i < 11; i++ {
		c.observe(Recessive, 11+i)
	}
	if c.Groups != 1 || c.RecessiveRun != 0 {
		t.Fatalf("first group did not close: %+v", c)
	}
	for i := 0; i < 10; i++ {
		c.observe(Recessive, 22+i)
	}
	c.observe(Dominant, 32)
	if c.Groups != 1 || c.RecessiveRun != 0 {
		t.Fatalf("completed group was lost on dominant: %+v", c)
	}
	// Groups are non-overlapping: 22 further recessive bits close exactly two.
	for i := 0; i < 22; i++ {
		c.observe(Recessive, 33+i)
	}
	if c.Groups != 3 || c.RecessiveRun != 0 {
		t.Fatalf("non-overlapping grouping wrong: %+v", c)
	}
	for i := 0; i < 125*11; i++ {
		c.observe(Recessive, 55+i)
	}
	if c.BusOff || c.TEC != 0 || c.RecoveryEnabled {
		t.Fatalf("state after the 128th group: %+v", c)
	}
	if c.RecoveredAt != 55+125*11-1 {
		t.Fatalf("recovered at %d", c.RecoveredAt)
	}
}
