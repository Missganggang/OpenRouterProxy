//go:build linux

package nodeclient

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func commandProcess(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	return cmd
}

func executeCommand(parent context.Context, command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := commandProcess(ctx, command)
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return output.String(), err
}

type linuxPTY struct {
	*os.File
	cmd    *exec.Cmd
	cancel context.CancelFunc
	once   sync.Once
}

func (p *linuxPTY) Close() error {
	var err error
	p.once.Do(func() {
		p.cancel()
		if p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
		err = p.File.Close()
	})
	return err
}
func (p *linuxPTY) Resize(cols, rows int) error {
	if cols < 1 || cols > 500 || rows < 1 || rows > 300 {
		return fmt.Errorf("invalid terminal dimensions")
	}
	return unix.IoctlSetWinsize(int(p.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(cols), Row: uint16(rows)})
}

func startPTY(parent context.Context, cols, rows int) (ptySession, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	master := os.NewFile(uintptr(fd), "/dev/ptmx")
	fail := func(err error) (ptySession, error) { master.Close(); return nil, err }
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return fail(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return fail(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	defer slave.Close()
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-i")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	pty := &linuxPTY{File: master, cmd: cmd, cancel: cancel}
	if err := pty.Resize(cols, rows); err != nil {
		cancel()
		return fail(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return fail(err)
	}
	// The PTY reader drains the final output before closing the master. Closing it
	// immediately after Wait could discard the shell's last buffered output.
	go func() { _ = cmd.Wait() }()
	return pty, nil
}
