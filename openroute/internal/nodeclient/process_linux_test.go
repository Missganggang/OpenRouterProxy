//go:build linux

package nodeclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRemoteCommandRunsAndTimesOutProcessGroup(t *testing.T) {
	output, err := executeCommand(context.Background(), "printf success", time.Second)
	if err != nil || output != "success" {
		t.Fatalf("remote exec: %q %v", output, err)
	}
	started := time.Now()
	_, err = executeCommand(context.Background(), "sleep 30 & wait", 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("process group did not terminate promptly: %v", err)
	}
}

func TestPTYHasTerminalResizeControlCAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pty, err := startPTY(ctx, 100, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()
	chunks := make(chan string, 128)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := pty.Read(buf)
			if n > 0 {
				chunks <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	readUntil := func(want string) {
		t.Helper()
		var all strings.Builder
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for {
			select {
			case chunk := <-chunks:
				all.WriteString(chunk)
				if strings.Contains(all.String(), want) {
					return
				}
			case <-timer.C:
				t.Fatalf("PTY missing %q; output=%q", want, all.String())
			case <-done:
				t.Fatalf("PTY exited before %q; output=%q", want, all.String())
			}
		}
	}
	if _, err := pty.Write([]byte("stty -echo; test -t 0 && printf 'TTY_%s\\n' OK\n")); err != nil {
		t.Fatal(err)
	}
	readUntil("TTY_OK")
	if err := pty.Resize(120, 50); err != nil {
		t.Fatal(err)
	}
	pty.Write([]byte("stty size\n"))
	readUntil("50 120")
	pty.Write([]byte("printf 'WAIT_%s\\n' READY; sleep 20\n"))
	readUntil("WAIT_READY")
	time.Sleep(50 * time.Millisecond)
	pty.Write([]byte{3})
	pty.Write([]byte("printf 'AFTER_%s\\n' INTERRUPT\n"))
	readUntil("AFTER_INTERRUPT")
	cancel()
	pty.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PTY close did not unblock reader")
	}
}

func TestPTYDrainsFinalOutput(t *testing.T) {
	pty, err := startPTY(context.Background(), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()
	pty.Write([]byte("stty -echo; printf 'FINAL_%s\\n' OUTPUT; exit\n"))
	done := make(chan string, 1)
	go func() {
		var output strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := pty.Read(buf)
			output.Write(buf[:n])
			if err != nil {
				done <- output.String()
				return
			}
		}
	}()
	select {
	case output := <-done:
		if !strings.Contains(output, "FINAL_OUTPUT") {
			t.Fatalf("last output discarded: %q", output)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PTY did not exit")
	}
}
