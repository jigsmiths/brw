package sandbox

import "io"

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
