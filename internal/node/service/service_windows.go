//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Install registers crowdvm-node as a Windows service (SCM, automatic
// start, runs without a logged-in user as LocalSystem, restarted on
// failure) and starts it unless NoStart. Registering a service needs an
// elevated prompt; with LogonTask (or when not elevated and LogonTask is
// allowed by the caller) a logon-time scheduled task of the current user
// is used instead, which needs no elevation but runs only while the user
// is logged in.
//
// The service's arguments must name the data dir explicitly: LocalSystem's
// %LOCALAPPDATA% is not the installing user's.
func Install(o Options, out io.Writer) error {
	if o.LogonTask {
		return installTask(o, out)
	}
	if o.DryRun {
		fmt.Fprintf(out, "# would create service %s: %s %s (automatic start, LocalSystem, restart on failure)\n", Name, o.Exe, strings.Join(o.Args, " "))
		return nil
	}
	m, err := mgr.Connect()
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("installing a Windows service needs an elevated (Administrator) prompt; " +
				"or run `crowdvm-node install-service --logon-task` to start the node at your logon instead")
		}
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(Name); err == nil { // reinstall: replace
		s.Control(svc.Stop)
		waitStopped(s)
		s.Delete()
		s.Close()
		time.Sleep(time.Second)
	}
	s, err := m.CreateService(Name, o.Exe, mgr.Config{
		DisplayName:  "CrowdVM node",
		Description:  "Hosts CrowdVM sandbox VMs for a CrowdVM server (crowdvm-node run).",
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	}, o.Args...)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}, 24*60*60)
	_ = s.SetRecoveryActionsOnNonCrashFailures(true) // also restart after a failed clean exit
	_ = uninstallTask(io.Discard)                    // the old logon task would start a second node
	fmt.Fprintf(out, "installed Windows service %s (automatic start)\n", Name)
	if o.NoStart {
		return nil
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	fmt.Fprintf(out, "started; logs: <data dir>\\node.log\n")
	return nil
}

// Uninstall stops and removes the service and any logon task.
func Uninstall(out io.Writer) error {
	var errs []error
	if m, err := mgr.Connect(); err == nil {
		if s, err := m.OpenService(Name); err == nil {
			s.Control(svc.Stop)
			waitStopped(s)
			if err := s.Delete(); err != nil {
				errs = append(errs, fmt.Errorf("delete service: %w", err))
			} else {
				fmt.Fprintf(out, "removed Windows service %s\n", Name)
			}
			s.Close()
		}
		m.Disconnect()
	}
	if err := uninstallTask(out); err == nil {
		fmt.Fprintf(out, "removed logon task %s\n", Name)
	}
	return errors.Join(errs...)
}

func waitStopped(s *mgr.Service) {
	for range 30 {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			return
		}
		time.Sleep(time.Second)
	}
}

// RunService runs fn as the Windows service when the process was started
// by the service manager (handled = true); fn's context ends on Stop or
// Shutdown. Otherwise it returns at once with handled = false.
func RunService(fn func(ctx context.Context) error) (handled bool, err error) {
	if ok, _ := svc.IsWindowsService(); !ok {
		return false, nil
	}
	h := &handler{fn: fn}
	if err := svc.Run(Name, h); err != nil {
		return true, err
	}
	return true, h.err
}

type handler struct {
	fn  func(ctx context.Context) error
	err error
}

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	st <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.fn(ctx) }()
	st <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case h.err = <-done:
			cancel()
			if errors.Is(h.err, ErrRestart) {
				// Exit without reporting SERVICE_STOPPED: the SCM counts that
				// as a crash and runs the recovery actions (restart in 5 s).
				// A clean stop with an exit code would only be recovered with
				// the non-crash-failures flag, which older installs lack.
				os.Exit(1)
			}
			if h.err != nil {
				return true, 1 // non-zero exit: the recovery actions restart it
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				st <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				st <- svc.Status{State: svc.StopPending, WaitHint: 30000}
				cancel()
				select {
				case h.err = <-done:
				case <-time.After(25 * time.Second):
				}
				return false, 0
			}
		}
	}
}

// --- logon scheduled task (no elevation) ---

func installTask(o Options, out io.Writer) error {
	tr := quote(o.Exe)
	for _, a := range o.Args {
		tr += " " + quote(a)
	}
	args := []string{"/Create", "/F", "/TN", Name, "/SC", "ONLOGON", "/RL", "LIMITED", "/TR", tr}
	if o.DryRun {
		fmt.Fprintf(out, "# would run: schtasks %s\n", strings.Join(args, " "))
		return nil
	}
	if err := schtasks(out, args...); err != nil {
		return err
	}
	if !o.NoStart {
		return schtasks(out, "/Run", "/TN", Name)
	}
	return nil
}

func uninstallTask(out io.Writer) error {
	if exec.Command("schtasks", "/Query", "/TN", Name).Run() != nil {
		return errors.New("no task")
	}
	_ = schtasks(out, "/End", "/TN", Name)
	return schtasks(out, "/Delete", "/F", "/TN", Name)
}

func quote(s string) string {
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func schtasks(out io.Writer, args ...string) error {
	cmd := exec.Command("schtasks", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("schtasks %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// RestartsOnExit is false outside RunService; inside it, returning
// ErrRestart exits like a crash and the recovery actions restart the node.
func RestartsOnExit() bool { return false }
