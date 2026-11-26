package sandbox

import "io"

// forward is brew's output thread and stdin thread: what the child prints goes to out and what the user types to the child.
// The terminal reports EIO once the child has closed its end, which is how the output ends, so the copy's error is dropped.
func forward(controller io.ReadWriter, in io.Reader, out io.Writer, passthroughStdin bool) <-chan struct{} {
	finished := make(chan struct{})
	if passthroughStdin {
		go func() { _, _ = io.Copy(controller, in) }()
	}
	go func() {
		defer close(finished)
		_, _ = io.Copy(out, controller)
	}()
	return finished
}
