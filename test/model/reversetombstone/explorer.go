package reversetombstone

import "fmt"

// ExplorationReport is an exact result for one finite set of Bounds. It is
// not an unbounded proof and callers must publish the bounds with the counts.
type ExplorationReport struct {
	States      int
	Transitions int
	MaxDepth    int
	Violation   error
	Trace       []Event
}

type explorationNode struct {
	State  State
	Depth  int
	Parent int
	Event  Event
}

// Explore performs breadth-first exhaustive exploration of every enabled
// transition up to MaxDepth, deduplicating exact abstract states.
func Explore(bounds Bounds) ExplorationReport {
	return explore(bounds, Faults{})
}

func explore(bounds Bounds, faults Faults) ExplorationReport {
	report := ExplorationReport{}
	if err := bounds.Validate(); err != nil {
		report.Violation = err
		return report
	}

	initial := InitialState(bounds)
	if err := ValidateState(initial, bounds); err != nil {
		report.Violation = fmt.Errorf("initial state: %w", err)
		return report
	}

	nodes := []explorationNode{{State: initial, Parent: -1}}
	seen := map[State]int{initial: 0}
	for cursor := 0; cursor < len(nodes); cursor++ {
		node := nodes[cursor]
		if node.Depth > report.MaxDepth {
			report.MaxDepth = node.Depth
		}
		if node.Depth >= bounds.MaxDepth {
			continue
		}
		for _, event := range CandidateEvents(node.State, bounds) {
			next, enabled, err := apply(node.State, event, bounds, faults)
			if !enabled {
				continue
			}
			report.Transitions++
			if err != nil {
				report.States = len(nodes)
				report.Violation = err
				report.Trace = append(reconstructTrace(nodes, cursor), event)
				return report
			}
			if _, exists := seen[next]; exists {
				continue
			}
			seen[next] = len(nodes)
			nodes = append(nodes, explorationNode{
				State:  next,
				Depth:  node.Depth + 1,
				Parent: cursor,
				Event:  event,
			})
		}
	}

	report.States = len(nodes)
	return report
}

func reconstructTrace(nodes []explorationNode, index int) []Event {
	reversed := make([]Event, 0, nodes[index].Depth)
	for index > 0 {
		node := nodes[index]
		reversed = append(reversed, node.Event)
		index = node.Parent
	}
	trace := make([]Event, len(reversed))
	for i := range reversed {
		trace[len(reversed)-1-i] = reversed[i]
	}
	return trace
}
