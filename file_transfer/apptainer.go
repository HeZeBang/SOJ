package file_transfer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/mrhaoxx/SOJ/types"
	"github.com/rs/zerolog/log"
)

type ApptainerService struct{}

func NewApptainerService() *ApptainerService {
	return &ApptainerService{}
}

func (s *ApptainerService) RunImage(name string, user string, hostname string, image string, workdir string, mounts []types.Mount, mask bool, ReadonlyRootfs bool, networkdisabled bool, timeout int, networkhosted bool, env []string) (ok bool, id string) {
	// Construction of apptainer instance start
	args := []string{"instance", "start"}

	if mask {
		args = append(args, "--no-mount", "sys,proc")
	}
	
	args = append(args, "--containall")

	if !ReadonlyRootfs {
		// SIF images are read-only by default; --writable-tmpfs adds an ephemeral overlay.
		args = append(args, "--writable-tmpfs")
	}

	if networkdisabled {
		args = append(args, "--net", "--network=none")
	} else if networkhosted {
		// Apptainer host network is default without --net, but with --containall it might disable net, we can omit --net
	}

	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}

	if workdir != "" {
		args = append(args, "--pwd", workdir)
	}

	for _, m := range mounts {
		bind := fmt.Sprintf("%s:%s", m.Source, m.Target)
		if m.ReadOnly {
			bind += ":ro"
		}
		args = append(args, "--bind", bind)
	}

	for _, e := range env {
		args = append(args, "--env", e)
	}

	args = append(args, image, name)

	// we use systemd-run --scope to constrain the entire instance?
	// if we do `systemd-run --scope --unit=soj-instance-id apptainer instance start ...`, systemd-run will exit when `apptainer instance start` completes!
	// Yes, `apptainer instance start` forks the daemon. Does it escape the systemd cgroup? 
	// Wait, systemd-run --scope with `RemainAfterExit=yes` is not valid for scope. But the background process is in the same cgroup scope. Wait, systemd might kill remaining processes when the scope main PID exits. 
	// Actually we should just run `apptainer exec` for EACH ExecContainer under `systemd-run`. This is much simpler and exactly maps to Systemd Cgroups (CPU, Memory limits apply per command exactly as Docker container). But what if state needs to be preserved?
	// State is preserved in /work anyway. Background daemon processes are rare in simple judges. 
	// Wait, if it's an instance, user runs `apptainer exec instance://...`.
	
	cmd := exec.Command("apptainer", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Err(err).Str("name", name).Str("output", string(out)).Msg("apptainer instance start failed")
		return false, ""
	}

	log.Debug().Str("name", name).Msg("apptainer instance started")
	return true, name
}

func (s *ApptainerService) CleanContainer(id string) {
	cmd := exec.Command("apptainer", "instance", "stop", id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Err(err).Str("id", id).Str("out", string(out)).Msg("apptainer instance stop failed")
	}
	log.Debug().Str("id", id).Msg("apptainer instance stopped")
}

func (s *ApptainerService) ExecContainer(id string, cmdStr string, timeout int, stdin io.Reader, stdout, stderr io.Writer, env []string, privileged bool) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	// Use systemd-run to launch apptainer exec instance://id.
	// ProtectSystem/ProtectHome are service-unit-only options and are silently
	// ignored for scope units, so they are intentionally omitted here.
	runArgs := []string{
		"--scope",
		"--quiet",
		fmt.Sprintf("--property=RuntimeMaxSec=%d", timeout),
		"--property=LimitMEMLOCK=0",
		"--property=NoNewPrivileges=yes",
		"apptainer", "exec",
	}

	for _, e := range env {
		runArgs = append(runArgs, "--env", e)
	}

	runArgs = append(runArgs, "instance://"+id, "sh", "-c", cmdStr)

	cmd := exec.CommandContext(ctx, "systemd-run", runArgs...)

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	
	if stdin != nil {
		cmd.Stdin = stdin
	}

	// We can use io.MultiWriter if stdout/stderr are provided
	if stdout != nil {
		cmd.Stdout = io.MultiWriter(stdout, &outBuf)
	} else {
		cmd.Stdout = &outBuf
	}

	if stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, &errBuf)
	} else {
		cmd.Stderr = &errBuf
	}

	err := cmd.Run()
	var exitCode int
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			exitCode = -1
		}
	} else {
		exitCode = 0
	}

	// combine logs for return
	logs := outBuf.String() + "\n" + errBuf.String()

	return exitCode, logs, err
}

func (s *ApptainerService) GetContainerLogs(id string) (string, error) {
	// Apptainer instances don't have built-in log fetcher like docker logs, unless we redirect stdout/stderr inside the instance.
	// For judge evaluator logic, we just return empty or whatever since output is collected during ExecContainer.
	return "Logs are collected step-by-step for apptainer", nil
}
