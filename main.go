package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--confinement" {
		if err := runConfinementFile(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	nodes := []*Node{
		NewNode("N1", func(f Frame) bool { return f.ID == 0x101 }),
		NewNode("N2", nil),
		NewNode("N3", nil),
		NewNode("N4", func(f Frame) bool { return f.ID != 0x440 }),
	}

	// The queue order deliberately is not identifier order. Arbitration has to
	// happen bit by bit after all three frames start together.
	nodes[0].Enqueue(Frame{ID: 0x540, Data: []byte{0x10, 0x20}, Enqueue: 0})
	nodes[1].Enqueue(Frame{ID: 0x480, Data: []byte{0x30}, Enqueue: 0})
	nodes[2].Enqueue(Frame{ID: 0x440, Data: []byte{0x40, 0x01}, Enqueue: 0})

	// This waits until all three simultaneous frames (and their follow-on
	// arbitration) have completed; it is not sorted into the initial arbitration.
	nodes[3].Enqueue(Frame{ID: 0x101, Data: []byte{0xaa}, Enqueue: 210})

	bus, err := NewBus(nodes, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	res, err := bus.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	printResult(res)
}

func printResult(r *Result) {
	fmt.Println("Bus bit trace (0=dominant, 1=recessive, .=idle/intermission)")
	for _, bit := range r.Trace {
		label := string(bit.Kind)
		if bit.Mismatch {
			label = "MISMATCH"
		}
		level := bit.Wire.String()
		if bit.Kind == KindIdle || bit.Kind == KindIntermission {
			level = "."
		}
		fmt.Printf("%3d %s %-15s", bit.Time, level, label)
		if len(bit.FaultSamples) > 0 {
			fmt.Printf(" fault=%v", bit.FaultSamples)
		}
		fmt.Println()
	}

	fmt.Println("\nArbitration exits")
	if len(r.ArbitrationExits) == 0 {
		fmt.Println("  none")
	}
	for _, x := range r.ArbitrationExits {
		if x.BitNumber > 0 {
			fmt.Printf("  %s exits at wire bit %d, ID bit %d, attempt %d\n",
				x.Node, x.WireBit, x.BitNumber, x.Attempt)
		} else {
			fmt.Printf("  %s exits at wire bit %d, field %s, attempt %d\n",
				x.Node, x.WireBit, x.Field, x.Attempt)
		}
	}

	fmt.Println("\nTransmission results")
	for _, x := range r.Transmissions {
		fmt.Printf("  %s ID=%#03x data=% X attempts=%d status=%s",
			x.Node, x.Frame.ID, x.Frame.Data, x.Attempts, x.Status)
		if x.Error != "" {
			fmt.Printf(" error=%q", x.Error)
		}
		fmt.Println()
	}

	fmt.Println("\nReceive results")
	for _, x := range r.Received {
		action := "accepted"
		if !x.Accepted {
			action = "filtered"
		}
		fmt.Printf("  %s %s ID=%#03x data=% X (ACKed regardless of filter)\n",
			x.Node, action, x.Frame.ID, x.Frame.Data)
	}

	fmt.Println("\nInvalid frame events")
	if len(r.Errors) == 0 {
		fmt.Println("  none")
	}
	for _, e := range r.Errors {
		fmt.Printf("  wire bit %d: %s, flag at %d, senders=%v attempt=%d\n",
			e.WireBit, e.Reason, e.FlagWireBit, e.Senders, e.Attempt)
	}
}
