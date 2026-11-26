package tasks

// SetDequeuedHook sets the hook a worker runs between taking a task off
// the queue and running it. Call it before Start.
func SetDequeuedHook(e *Engine, f func(id string)) { e.dequeued = f }
