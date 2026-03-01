package bgprocess

import (
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const ringBufferSize = 64 * 1024 // 64KB

// Process wraps an exec.Cmd with merged stdout/stderr capture into a ring buffer.
type Process struct {
	ID         string
	InstanceID string
	Name       string
	Command    string
	Port       int
	Pid        int
	Status     string // "running", "stopped", "errored"
	StartedAt  time.Time

	cmd    *exec.Cmd
	buf    *ringBuffer
	mu     sync.Mutex
	doneCh chan struct{}
}

// Start launches the process.
func (p *Process) Start() error {
	p.cmd = exec.Command("sh", "-c", p.Command)
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	pr, pw := io.Pipe()
	p.cmd.Stdout = pw
	p.cmd.Stderr = pw

	p.buf = newRingBuffer(ringBufferSize)
	p.doneCh = make(chan struct{})

	if err := p.cmd.Start(); err != nil {
		return err
	}

	p.Pid = p.cmd.Process.Pid
	p.Status = "running"
	p.StartedAt = time.Now()

	// Read output into ring buffer.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				p.buf.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}()

	// Wait for process exit.
	go func() {
		err := p.cmd.Wait()
		pw.Close()
		p.mu.Lock()
		if err != nil {
			p.Status = "errored"
		} else {
			p.Status = "stopped"
		}
		p.mu.Unlock()
		close(p.doneCh)
	}()

	return nil
}

// Stop sends SIGTERM, waits 5s, then SIGKILL if needed.
func (p *Process) Stop() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}

	// Send SIGTERM to process group.
	syscall.Kill(-p.Pid, syscall.SIGTERM)

	select {
	case <-p.doneCh:
		return
	case <-time.After(5 * time.Second):
	}

	// Force kill.
	syscall.Kill(-p.Pid, syscall.SIGKILL)
	<-p.doneCh
}

// Output returns the current ring buffer contents.
func (p *Process) Output() []byte {
	return p.buf.Bytes()
}

// Done returns a channel that closes when the process exits.
func (p *Process) Done() <-chan struct{} {
	return p.doneCh
}

// IsRunning reports whether the process is still running.
func (p *Process) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Status == "running"
}

// ringBuffer is a simple fixed-size circular buffer.
type ringBuffer struct {
	mu   sync.Mutex
	data []byte
	size int
	pos  int
	full bool

	// Callback for new data.
	onWrite func([]byte)
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{
		data: make([]byte, size),
		size: size,
	}
}

func (rb *ringBuffer) SetOnWrite(fn func([]byte)) {
	rb.mu.Lock()
	rb.onWrite = fn
	rb.mu.Unlock()
}

func (rb *ringBuffer) Write(p []byte) {
	rb.mu.Lock()
	fn := rb.onWrite
	for _, b := range p {
		rb.data[rb.pos] = b
		rb.pos = (rb.pos + 1) % rb.size
		if rb.pos == 0 {
			rb.full = true
		}
	}
	rb.mu.Unlock()

	if fn != nil {
		fn(p)
	}
}

func (rb *ringBuffer) Bytes() []byte {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if !rb.full {
		out := make([]byte, rb.pos)
		copy(out, rb.data[:rb.pos])
		return out
	}

	out := make([]byte, rb.size)
	n := copy(out, rb.data[rb.pos:])
	copy(out[n:], rb.data[:rb.pos])
	return out
}
