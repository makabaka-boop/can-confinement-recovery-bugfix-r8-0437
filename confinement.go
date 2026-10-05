package main

import "fmt"

type ConfinementState struct {
	TEC             int
	BusOff          bool
	RecoveryEnabled bool
	Groups          int
	RecessiveRun    int
	RecoveredAt     int
	RecoverAt       *int
}

func NewConfinement(tec int, recoverAt *int) (*ConfinementState, error) {
	if tec < 0 || tec > 256 {
		return nil, fmt.Errorf("initial TEC must be 0..256")
	}
	return &ConfinementState{TEC: tec, BusOff: tec >= 256, RecoveredAt: -1, RecoverAt: recoverAt}, nil
}
func (c *ConfinementState) failed() {
	if c.BusOff {
		return
	}
	c.TEC += 8
	if c.TEC >= 256 {
		// Saturation: the transmit error counter pins at 256 and the node
		// enters bus-off. Recovery can only start from an explicit request.
		c.TEC = 256
		c.BusOff = true
		c.RecoveryEnabled = false
		c.Groups = 0
		c.RecessiveRun = 0
	}
}
func (c *ConfinementState) succeeded() {
	if !c.BusOff && c.TEC > 0 {
		c.TEC--
	}
}
func (c *ConfinementState) requestRecovery() {
	if !c.BusOff || c.RecoveryEnabled {
		return
	}
	// Observation starts only at this explicit request: bits seen before it,
	// including idle bits while bus-off, never count.
	c.RecoveryEnabled = true
	c.Groups = 0
	c.RecessiveRun = 0
}
func (c *ConfinementState) observe(wire BitLevel, at int) {
	if !c.BusOff || !c.RecoveryEnabled {
		return
	}
	if wire == Dominant {
		// A dominant bit interrupts only the current, not-yet-complete group;
		// already completed 11-bit groups are retained.
		c.RecessiveRun = 0
		return
	}
	c.RecessiveRun++
	if c.RecessiveRun < 11 {
		return
	}
	// Exactly eleven recessive bits close one non-overlapping group; counting
	// the next group starts with the following wire bit.
	c.Groups++
	c.RecessiveRun = 0
	if c.Groups >= 128 {
		c.BusOff = false
		c.TEC = 0
		c.RecoveryEnabled = false
		c.RecoveredAt = at
	}
}
func (n *Node) blocked() bool { return n.Confinement != nil && n.Confinement.BusOff }
func (b *Bus) enableRecoveryRequests() {
	for _, n := range b.nodes {
		c := n.Confinement
		if c != nil && c.RecoverAt != nil && *c.RecoverAt == b.t {
			c.requestRecovery()
		}
	}
}
func (b *Bus) RunConfinement(limit int) (*Result, error) {
	if limit < 1 || limit > 20000 {
		return nil, fmt.Errorf("limit must be 1..20000 bits")
	}
	b.bitLimit = limit
	return b.Run()
}
