//go:build unix

package sshutil

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/waittest"
)

func startSupervised(t *testing.T, shell, agent string, stdin io.Reader, stdout *os.File) *exec.Cmd {
	t.Helper()

	sshdStandIn := exec.Command(shell, "-c", `"$0" -c "$1"; exit "$?"`, shell, supervisedAgent("{ "+agent+"\n}", time.Second))
	sshdStandIn.Stdin = stdin
	sshdStandIn.Stdout = stdout
	sshdStandIn.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sshdStandIn.Start(); err != nil {
		t.Fatalf("start %s: %v", shell, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-sshdStandIn.Process.Pid, syscall.SIGKILL) })
	return sshdStandIn
}

func waitExit(t *testing.T, cmd *exec.Cmd) error {
	t.Helper()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return waittest.Receive(t, exited, "supervised shell exit")
}

func TestSupervisedAgent_PassesStdinAndExitStatus(t *testing.T) {
	t.Parallel()

	for _, shell := range availableShells(t) {
		t.Run(shell.name, func(t *testing.T) {
			t.Parallel()

			cmd := startSupervised(t, shell.path, `read code; exit "$code"`, strings.NewReader("7\n"), nil)

			var exitErr *exec.ExitError
			if err := waitExit(t, cmd); !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
				t.Fatalf("exit = %v, want exit status 7", err)
			}
		})
	}
}

func TestSupervisedAgent_EndsTheGroupOnceTheParentIsGone(t *testing.T) {
	t.Parallel()

	for _, shell := range availableShells(t) {
		t.Run(shell.name, func(t *testing.T) {
			t.Parallel()

			agentOut, agentOutWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = agentOut.Close() })
			cmd := startSupervised(t, shell.path, "echo started; exec sleep 600", nil, agentOutWriter)
			_ = agentOutWriter.Close()

			started, ended := make(chan struct{}), make(chan struct{})
			go func() {
				r := bufio.NewReader(agentOut)
				_, _ = r.ReadString('\n')
				close(started)
				_, _ = io.Copy(io.Discard, r)
				close(ended)
			}()
			waittest.Receive(t, started, "agent start")

			_ = cmd.Process.Kill()
			_ = waitExit(t, cmd)
			waittest.Receive(t, ended, "agent end")
		})
	}
}
