package main

import (
	"fmt"
	"sort"
)

const maxAttempts = 3

// Error reasons reported in the simulation result.
const (
	reasonBit   = "bit error"
	reasonACK   = "missing ACK"
	reasonCRC   = "CRC error"
	reasonStuff = "stuff error"
	reasonForm  = "form error"
)

type busPhase int

const (
	phaseIdle busPhase = iota
	phaseFrame
	phaseErrorFlag
	phaseErrorDelimiter
	phaseIntermission
)

// AcceptFunc decides whether a correctly received frame is delivered to a
// node's application. It is not part of ACK handling.
type AcceptFunc func(Frame) bool

// Node has one FIFO of data frames. Node identity never affects arbitration;
// only the bits actually placed on the wire do.
type Node struct {
	Confinement *ConfinementState
	Name        string
	Accept      AcceptFunc
	queue       []Frame
	tx          *txJob
	recv        *receiver
}

// NewNode creates a node. A nil accept function accepts every frame.
func NewNode(name string, accept AcceptFunc) *Node {
	return &Node{Name: name, Accept: accept, recv: newReceiver()}
}

// Enqueue adds a frame to the node's FIFO at an integer bit time.
func (n *Node) Enqueue(f Frame) {
	n.queue = append(n.queue, f)
}

type txJob struct {
	frame   Frame
	encoded EncodedFrame
	attempt int
	active  bool
	lost    bool
	bit     int // this job's own position in its encoded frame
}

// ReceiverBitFault overrides only the RXD sample seen by ReceiverNode at the
// absolute physical wire bit AtWireBit. It does not change the wired-AND level.
type ReceiverBitFault struct {
	ReceiverNode string
	AtWireBit    int
	Level        BitLevel
}

// TraceBit is one time step on the shared virtual bus.
type TraceBit struct {
	Time         int
	Wire         BitLevel
	Kind         BitKind
	Drivers      map[string]BitLevel
	FaultSamples map[string]BitLevel
	Mismatch     bool
}

// ArbitrationExit records the exact physical bit at which a sender lost.
type ArbitrationExit struct {
	Node      string
	Frame     Frame
	WireBit   int
	Attempt   int
	Field     string
	BitNumber int // 1 is identifier MSB; -1 outside the identifier
}

// ErrorEvent marks the bit where a condition made the current frame invalid.
type ErrorEvent struct {
	WireBit     int
	Reason      string
	Frame       Frame
	Senders     []string
	Attempt     int
	FlagWireBit int
}

// TransmitRecord is the final outcome of one enqueued frame at one node.
type TransmitRecord struct {
	Node     string
	Frame    Frame
	Attempts int
	Status   string
	Error    string
}

// ReceiveRecord is one frame made visible to an application. Filtered frames
// were still ACKed correctly.
type ReceiveRecord struct {
	Node     string
	Frame    Frame
	WireBit  int
	Accepted bool
}

// Result contains the requested observable simulation artifacts.
type Result struct {
	Confinement      map[string]ConfinementState
	Pending          map[string]int
	Trace            []TraceBit
	ArbitrationExits []ArbitrationExit
	Errors           []ErrorEvent
	Transmissions    []TransmitRecord
	Received         []ReceiveRecord
}

type Bus struct {
	bitLimit int
	nodes    []*Node
	faults   []ReceiverBitFault
	byName   map[string]*Node

	phase     busPhase
	t         int
	phaseLeft int
	starters  []string
	joinedErr bool

	pendingFlagAt  int
	pendingReason  string
	pendingFrame   Frame
	pendingAt      int
	pendingSenders []string
	pendingAttempt int

	txResults []TransmitRecord
	exits     []ArbitrationExit
	errEvents []ErrorEvent
	received  []ReceiveRecord
	trace     []TraceBit
}

func NewBus(nodes []*Node, faults []ReceiverBitFault) (*Bus, error) {
	b := &Bus{
		nodes:         nodes,
		faults:        faults,
		byName:        make(map[string]*Node),
		pendingFlagAt: -1,
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if n.Name == "" || seen[n.Name] {
			return nil, fmt.Errorf("node names must be non-empty and unique")
		}
		seen[n.Name] = true
		b.byName[n.Name] = n
	}
	for _, f := range faults {
		if b.byName[f.ReceiverNode] == nil {
			return nil, fmt.Errorf("unknown fault receiver %q", f.ReceiverNode)
		}
	}
	return b, nil
}

func (b *Bus) Run() (*Result, error) {
	for guard := 0; guard < 100000; guard++ {
		b.enableRecoveryRequests()
		if b.bitLimit > 0 && b.t >= b.bitLimit {
			return b.result(), nil
		}
		switch b.phase {
		case phaseIdle:
			if b.done() {
				return b.result(), nil
			}
			if !b.startReadyFrames() {
				b.emit(Recessive, KindIdle, nil, nil, false)
			}
		case phaseFrame:
			if err := b.stepFrame(); err != nil {
				return nil, err
			}
		case phaseErrorFlag:
			b.stepErrorFlag()
		case phaseErrorDelimiter:
			b.stepFixedRecessive(phaseIntermission, 3, KindErrorDelimiter)
		case phaseIntermission:
			b.stepFixedRecessive(phaseIdle, 0, KindIntermission)
		}
	}
	return nil, fmt.Errorf("simulation exceeded maximum bit count")
}

func (b *Bus) done() bool {
	for _, n := range b.nodes {
		if n.blocked() {
			continue
		}
		if n.tx != nil || len(n.queue) > 0 {
			return false
		}
	}
	return true
}

func (b *Bus) startReadyFrames() bool {
	ready := false
	for _, n := range b.nodes {
		if n.blocked() {
			continue
		}
		if n.tx != nil {
			// Retransmission after an error, or automatic retry after losing
			// arbitration: the retained job starts without a queue pop.
			n.tx.active = true
			n.tx.lost = false
			n.tx.bit = 0
			ready = true
			continue
		}
		if len(n.queue) > 0 && n.queue[0].Enqueue <= b.t {
			f := n.queue[0]
			n.tx = &txJob{frame: f, encoded: EncodeDataFrame(f), attempt: 1, active: true, bit: 0}
			ready = true
		}
	}
	if !ready {
		return false
	}
	for _, n := range b.nodes {
		n.recv.reset()
	}
	b.phase = phaseFrame
	return true
}

func (b *Bus) activeNodes() []*Node {
	var out []*Node
	for _, n := range b.nodes {
		if n.tx != nil && n.tx.active {
			out = append(out, n)
		}
	}
	return out
}

// receivingNodes are controllers that may ACK or signal receiver errors at this
// frame. This includes controllers that lost arbitration, switched to receive,
// and kept their frame queued for automatic retransmission.
func (b *Bus) receivingNodes() []*Node {
	var out []*Node
	for _, n := range b.nodes {
		if n.tx == nil || !n.tx.active {
			out = append(out, n)
		}
	}
	return out
}

func (b *Bus) stepFrame() error {
	// A previous bit may already have deactivated the current transmitters and
	// scheduled the error flag; service that flag before requiring an active job.
	if b.pendingFlagAt == b.t {
		b.beginErrorFlag(b.t, b.pendingReason, b.pendingFrame, b.pendingAt,
			b.starters, b.pendingSenders, b.pendingAttempt)
		return nil
	}

	active := b.activeNodes()
	if len(active) == 0 {
		return fmt.Errorf("frame phase without active sender at bit %d", b.t)
	}
	first := active[0]
	expected := first.tx.encoded.Bits[first.tx.bit]
	kind := expected.Kind

	// A CRC failure is signalled in the ACK delimiter, even if the frame was
	// ACKed by a different receiver. Schedule it for *this* bit via the normal
	// pending path so an already-pending condition (e.g. missing ACK at the
	// preceding slot) is not overwritten.
	if kind == KindACKDelim && b.pendingFlagAt < 0 {
		var crcBad []string
		for _, n := range b.receivingNodes() {
			if n.recv.crcError {
				crcBad = append(crcBad, n.Name)
			}
		}
		if len(crcBad) > 0 {
			b.scheduleErrorAt(reasonCRC, b.t, b.t, uniqueSorted(crcBad))
			return b.stepFrame()
		}
	}

	drivers := map[string]BitLevel{}
	for _, n := range active {
		drivers[n.Name] = n.tx.encoded.Bits[n.tx.bit].Level
	}
	if kind == KindACKSlot {
		for _, n := range b.receivingNodes() {
			if n.recv.canAck() {
				drivers[n.Name] = Dominant
			}
		}
	}
	wire := wiredAND(drivers)

	faultSamples := map[string]BitLevel{}
	samples := map[string]BitLevel{}
	for _, n := range b.nodes {
		sample := wire
		for _, fault := range b.faults {
			if fault.AtWireBit == b.t && fault.ReceiverNode == n.Name {
				// A fixture may corrupt the sampled bit at any controller,
				// including one that is transmitting. An active sender whose
				// recessive sample reads dominant outside arbitration raises a
				// bit error; the wired-AND level itself is unchanged.
				sample = fault.Level
				faultSamples[n.Name] = sample
			}
		}
		samples[n.Name] = sample
	}

	mismatch := false
	for _, n := range active {
		bit := n.tx.encoded.Bits[n.tx.bit]
		// Recessive-vs-dominant during ID/RTR is arbitration, not an error.
		arbitration := bit.Kind == KindID || bit.Kind == KindRTR
		if !arbitration && bit.Kind != KindACKSlot && drivers[n.Name] != samples[n.Name] {
			mismatch = true
		}
	}
	b.emit(wire, kind, mapTraceDrivers(drivers), faultSamples, mismatch)

	for _, n := range b.nodes {
		n.recv.feed(samples[n.Name], kind)
	}

	bitErrorStarter := ""
	var losers []*Node
	for _, n := range active {
		bit := n.tx.encoded.Bits[n.tx.bit]
		if bit.Level == Recessive && samples[n.Name] == Dominant {
			if bit.Kind == KindID || bit.Kind == KindRTR {
				losers = append(losers, n)
			} else if bit.Kind != KindACKSlot {
				bitErrorStarter = n.Name
				break
			}
		}
	}
	for _, n := range losers {
		b.loseArbitration(n)
	}

	for _, n := range active {
		if n.tx != nil {
			n.tx.bit++
		}
	}
	if bitErrorStarter != "" {
		b.scheduleError(reasonBit, b.t-1, []string{bitErrorStarter})
	} else {
		// A form error on the bit just consumed begins its error flag at this
		// same bit; a stuff error is signalled on the following bit.
		var immediateNode string
		for _, n := range b.receivingNodes() {
			if n.recv.takeImmediateError() {
				immediateNode = n.Name
			}
		}
		if immediateNode != "" {
			b.scheduleErrorAt(reasonForm, b.t-1, b.t, []string{immediateNode})
		} else {
			for _, n := range b.receivingNodes() {
				if reason := n.recv.consumeNextBitError(); reason != "" {
					b.scheduleError(reason, b.t-1, []string{n.Name})
					break
				}
			}
		}
	}

	if b.pendingFlagAt < 0 && kind == KindACKSlot && wire == Recessive {
		b.scheduleError(reasonACK, b.t-1, activeNames(active))
	}

	if b.pendingFlagAt >= 0 {
		return nil
	}
	if first.tx.bit == len(first.tx.encoded.Bits) {
		b.finishGoodFrame(active)
	}
	return nil
}

func (b *Bus) loseArbitration(n *Node) {
	if n.Confinement != nil {
		n.Confinement.failed()
	}
	n.tx.active = false
	n.tx.lost = true
	bit := n.tx.encoded.Bits[n.tx.bit]
	exit := ArbitrationExit{
		Node:      n.Name,
		Frame:     n.tx.frame,
		WireBit:   b.t - 1,
		Attempt:   n.tx.attempt,
		BitNumber: -1,
	}
	if bit.Kind == KindID {
		exit.Field = "ID"
		exit.BitNumber = bit.Index // raw index 1..11, where 1 is the MSB
	} else {
		exit.Field = string(bit.Kind)
	}
	b.exits = append(b.exits, exit)
}

func (b *Bus) scheduleError(reason string, detectedAt int, starters []string) {
	b.scheduleErrorAt(reason, detectedAt, detectedAt+1, starters)
}

func (b *Bus) scheduleErrorAt(reason string, detectedAt, flagAt int, starters []string) {
	if b.pendingFlagAt >= 0 {
		return
	}
	active := b.activeNodes()
	if len(active) == 0 {
		return
	}
	b.pendingReason = reason
	b.pendingFlagAt = flagAt
	b.pendingFrame = active[0].tx.frame
	b.pendingAt = detectedAt
	b.pendingSenders = activeNames(active)
	b.pendingAttempt = active[0].tx.attempt
	b.starters = uniqueSorted(starters)
	b.failActiveJobs(reason)
}

func (b *Bus) failActiveJobs(_ string) {
	for _, n := range b.nodes {
		if n.tx == nil || !n.tx.active {
			continue
		}
		job := n.tx
		job.active = false
		if n.Confinement != nil {
			n.Confinement.failed()
			if n.blocked() {
				n.queue = n.queue[1:]
				n.tx = nil
				continue
			}
		}
		if job.attempt < maxAttempts {
			job.attempt++
		} else {
			b.txResults = append(b.txResults, TransmitRecord{
				Node:     n.Name,
				Frame:    job.frame,
				Attempts: job.attempt,
				Status:   "failed",
				Error:    reasonFromPending(b),
			})
			n.queue = n.queue[1:]
			n.tx = nil
		}
	}
}

func reasonFromPending(b *Bus) string {
	if b.pendingReason != "" {
		return b.pendingReason
	}
	return reasonCRC
}

func (b *Bus) beginErrorFlag(at int, reason string, frame Frame, detectedAt int, starters, senders []string, attempt int) {
	b.phase = phaseErrorFlag
	b.phaseLeft = 6
	b.starters = uniqueSorted(starters)
	b.joinedErr = false
	b.pendingFlagAt = -1
	b.pendingReason = ""
	for _, n := range b.nodes {
		n.recv.reset()
	}
	b.errEvents = append(b.errEvents, ErrorEvent{
		WireBit:     detectedAt,
		Reason:      reason,
		Frame:       frame,
		Senders:     append([]string(nil), senders...),
		Attempt:     attempt,
		FlagWireBit: at,
	})
	b.stepErrorFlag()
}

func (b *Bus) stepErrorFlag() {
	drivers := map[string]BitLevel{}
	if b.joinedErr {
		for _, n := range b.nodes {
			drivers[n.Name] = Dominant
		}
	} else {
		for _, name := range b.starters {
			drivers[name] = Dominant
		}
	}
	b.emit(Dominant, KindErrorFlag, mapTraceDrivers(drivers), nil, false)
	b.joinedErr = true
	b.phaseLeft--
	if b.phaseLeft == 0 {
		b.phase = phaseErrorDelimiter
		b.phaseLeft = 8
	}
}

func (b *Bus) stepFixedRecessive(next busPhase, nextLeft int, kind BitKind) {
	drivers := map[string]BitLevel{}
	for _, n := range b.nodes {
		drivers[n.Name] = Recessive
	}
	b.emit(Recessive, kind, mapTraceDrivers(drivers), nil, false)
	b.phaseLeft--
	if b.phaseLeft == 0 {
		b.phase = next
		b.phaseLeft = nextLeft
	}
}

func (b *Bus) finishGoodFrame(active []*Node) {
	f := active[0].tx.frame
	lastBit := b.t - 1
	senders := map[string]bool{}
	for _, n := range active {
		senders[n.Name] = true
	}
	for _, n := range b.nodes {
		// A controller that lost arbitration continues receiving. A controller
		// that was an active sender does not receive its own frame.
		if senders[n.Name] {
			continue
		}
		if !n.recv.delivered() {
			continue
		}
		accepted := n.Accept == nil || n.Accept(f)
		b.received = append(b.received, ReceiveRecord{
			Node:     n.Name,
			Frame:    f,
			WireBit:  lastBit,
			Accepted: accepted,
		})
	}
	for _, n := range active {
		if n.Confinement != nil {
			n.Confinement.succeeded()
		}
		b.txResults = append(b.txResults, TransmitRecord{
			Node:     n.Name,
			Frame:    n.tx.frame,
			Attempts: n.tx.attempt,
			Status:   "ok",
		})
		n.queue = n.queue[1:]
		n.tx = nil
	}
	b.phase = phaseIntermission
	b.phaseLeft = 3
}

func (b *Bus) emit(wire BitLevel, kind BitKind, drivers map[string]BitLevel, faults map[string]BitLevel, mismatch bool) {
	if drivers == nil {
		drivers = map[string]BitLevel{}
	}
	if faults == nil {
		faults = map[string]BitLevel{}
	}
	b.trace = append(b.trace, TraceBit{
		Time:         b.t,
		Wire:         wire,
		Kind:         kind,
		Drivers:      drivers,
		FaultSamples: faults,
		Mismatch:     mismatch,
	})
	for _, n := range b.nodes {
		if n.Confinement != nil {
			n.Confinement.observe(wire, b.t)
		}
	}
	b.t++
}

func wiredAND(drivers map[string]BitLevel) BitLevel {
	wire := Recessive
	for _, level := range drivers {
		if level == Dominant {
			wire = Dominant
		}
	}
	return wire
}

func activeNames(nodes []*Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	sort.Strings(out)
	return out
}

func uniqueSorted(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func mapTraceDrivers(drivers map[string]BitLevel) map[string]BitLevel {
	out := make(map[string]BitLevel, len(drivers))
	for k, v := range drivers {
		out[k] = v
	}
	return out
}

func (b *Bus) result() *Result {
	states := map[string]ConfinementState{}
	pending := map[string]int{}
	for _, n := range b.nodes {
		if n.Confinement != nil {
			states[n.Name] = *n.Confinement
		}
		pending[n.Name] = len(n.queue)
	}
	return &Result{
		Confinement: states, Pending: pending,
		Trace:            b.trace,
		ArbitrationExits: b.exits,
		Errors:           b.errEvents,
		Transmissions:    b.txResults,
		Received:         b.received,
	}
}
