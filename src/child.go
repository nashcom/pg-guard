// pg-guard -- child.go -- fans out a tracked child's one-shot exit event to
// multiple observers. The channel returned by trackChild delivers its
// single value to exactly one receiver; both the main select loop (crash
// detection) and coordinateHandover (confirming a requested stop) need to
// observe the same exit, so watchChild wraps it in a close() instead --
// unlike a channel send, close() is a broadcast that every simultaneous
// receiver observes.

package main

type childWatcher struct {
	done chan struct{}
	code int
}

func watchChild(exited <-chan int) *childWatcher {
	cw := &childWatcher{done: make(chan struct{})}
	go func() {
		cw.code = <-exited
		close(cw.done)
	}()
	return cw
}
