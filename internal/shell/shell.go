package shell

import (
	"encoding/base64"
	"io"
	"log"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// Session wraps a PTY-backed shell process.
type Session struct {
	instanceID string
	ptmx       *os.File
	cmd        *exec.Cmd
	onOutput   func(instanceID, data string)
	onExit     func(instanceID, reason string)
	done       chan struct{}
}

// NewSession starts a login shell with a PTY.
func NewSession(instanceID, cwd string, cols, rows int, onOutput func(string, string), onExit func(string, string)) (*Session, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}

	cmd := exec.Command(shell, "-l")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}

	_ = pty.Setsize(ptmx, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})

	s := &Session{
		instanceID: instanceID,
		ptmx:       ptmx,
		cmd:        cmd,
		onOutput:   onOutput,
		onExit:     onExit,
		done:       make(chan struct{}),
	}

	go s.readLoop()
	go s.waitLoop()

	return s, nil
}

func (s *Session) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			encoded := base64.StdEncoding.EncodeToString(buf[:n])
			s.onOutput(s.instanceID, encoded)
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[shell %s] read error: %v", s.instanceID, err)
			}
			return
		}
	}
}

func (s *Session) waitLoop() {
	err := s.cmd.Wait()
	close(s.done)
	reason := "exited"
	if err != nil {
		reason = err.Error()
	}
	s.ptmx.Close()
	s.onExit(s.instanceID, reason)
}

// Write sends base64-encoded data to the shell.
func (s *Session) Write(b64data string) error {
	data, err := base64.StdEncoding.DecodeString(b64data)
	if err != nil {
		return err
	}
	_, err = s.ptmx.Write(data)
	return err
}

// Resize changes the PTY window size.
func (s *Session) Resize(cols, rows int) {
	_ = pty.Setsize(s.ptmx, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
}

// Close kills the shell process and closes the PTY.
func (s *Session) Close() {
	select {
	case <-s.done:
		return // already exited
	default:
	}
	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	s.ptmx.Close()
}
