//go:build !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe to read while the child process is still
// writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSetupInterruptsWhileBlockedOnInput covers Ctrl+C at a prompt in the
// real process.
//
// The child is given a partial line, so it is blocked inside the read. A
// SIGINT must end it promptly; a setup that only cancels a context without
// interrupting the read would stay alive until a newline arrived.
func TestSetupInterruptsWhileBlockedOnInput(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go toolchain is not available")
	}

	bin := filepath.Join(t.TempDir(), "themailroom")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the command: %v\n%s", err, out)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "themailroom.yaml")

	stdin, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer stdinWriter.Close()

	var output syncBuffer
	cmd := exec.Command(bin, "setup", "-config", path)
	cmd.Stdin = stdin
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = stdin.Close()

	// A partial line: the child cannot complete the prompt.
	if _, err := stdinWriter.Write([]byte("work\nwork@example.test\nmail.")); err != nil {
		t.Fatalf("write partial input: %v", err)
	}
	// Give the child time to reach the blocked read.
	time.Sleep(300 * time.Millisecond)

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			t.Errorf("setup exited 0 after SIGINT; output:\n%s", output.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("setup did not exit within 5s of SIGINT; it is blocked on input:\n%s", output.String())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("an interrupted setup left files behind: %v", names)
	}
}
