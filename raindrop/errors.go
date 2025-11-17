package raindrop

import "fmt"

var (
	// ErrQueueFull indicates that the in-memory buffer rejected the event.
	ErrQueueFull = fmt.Errorf("raindrop: event dropped, queue is full")

	// ErrInteractionFinished indicates Finish was already called.
	ErrInteractionFinished = fmt.Errorf("raindrop: interaction already finished")
)

// ValidationError is returned when user input violates SDK contracts.
type ValidationError struct {
	Field string
	Msg   string
}

func (e ValidationError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("raindrop: %s", e.Msg)
	}
	return fmt.Sprintf("raindrop: invalid %s: %s", e.Field, e.Msg)
}
