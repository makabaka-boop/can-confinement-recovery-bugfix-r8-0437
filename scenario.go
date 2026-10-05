package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type ScenarioFrame struct {
	ID      uint16
	Data    []int
	Enqueue int
}
type ScenarioNode struct {
	Name      string
	TEC       int
	RecoverAt *int
	Frames    []ScenarioFrame
}
type Scenario struct {
	Nodes  []ScenarioNode
	Faults []ReceiverBitFault
	Limit  int
}

func runConfinementFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var input Scenario
	if err = json.Unmarshal(raw, &input); err != nil {
		return err
	}
	nodes := []*Node{}
	for _, item := range input.Nodes {
		node := NewNode(item.Name, nil)
		node.Confinement, err = NewConfinement(item.TEC, item.RecoverAt)
		if err != nil {
			return err
		}
		if item.RecoverAt != nil && (*item.RecoverAt < 0 || *item.RecoverAt >= input.Limit) {
			return fmt.Errorf("recovery time outside limit")
		}
		for _, entry := range item.Frames {
			if entry.ID > 2047 || len(entry.Data) > 8 || entry.Enqueue < 0 {
				return fmt.Errorf("invalid frame")
			}
			data := make([]byte, len(entry.Data))
			for i, v := range entry.Data {
				if v < 0 || v > 255 {
					return fmt.Errorf("invalid byte")
				}
				data[i] = byte(v)
			}
			node.Enqueue(Frame{ID: int(entry.ID), Data: data, Enqueue: entry.Enqueue})
		}
		nodes = append(nodes, node)
	}
	bus, err := NewBus(nodes, input.Faults)
	if err != nil {
		return err
	}
	result, err := bus.RunConfinement(input.Limit)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
