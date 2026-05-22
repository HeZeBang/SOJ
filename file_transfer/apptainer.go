package file_transfer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/mrhaoxx/SOJ/types"
	"github.com/rs/zerolog/log"
)

const emptyMaskDir = "/var/lib/soj/empty-mask"

type ApptainerService struct {
	mu       sync.Mutex
	workdirs map[string]string // instanceID -> workdir inside container
}

func NewApptainerService() *ApptainerService {
	// Pre-create an empty directory used as the bind source when masking container dirs.
	// 0555 prevents the container from writing to it even if the bind were rw.
	if err := os.MkdirAll(emptyMaskDir, 0555); err != nil {
		log.Err(err).Str("path", emptyMaskDir).Msg("failed to create empty mask dir")
	}
	return &ApptainerService{workdirs: make(map[string]string)}
}

func (s *ApptainerService) RunImage(name string, user string, hostname string, image string, workdir string, mounts []types.Mount, maskFiles []string, maskDirs []string, ReadonlyRootfs bool, networkdisabled bool, timeout int, networkhosted bool, env []string) (ok bool, id string) {
	args := []string{"instance", "start"}

	// Mask sensitive paths by bind-mounting harmless sources over them.
	// Files: /dev/null overlays the path with a character device.
	// Dirs: an empty host directory overlays the path, hiding everything inside.
	for _, f := range maskFiles {
		args = append(args, "--bind", "/dev/null:"+f+":ro")
	}
	for _, d := range maskDirs {
		args = append(args, "--bind", emptyMaskDir+":"+d+":ro")
	}

	args = append(args, "--containall")

	if !ReadonlyRootfs {
		// SIF images are read-only by default; --writable-tmpfs adds an ephemeral overlay.
		args = append(args, "--writable-tmpfs")
	}

	if networkdisabled {
		args = append(args, "--net", "--network=none")
	}

	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}

	// --pwd is not supported by instance start; workdir is passed at exec time instead.

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

	cmd := exec.Command("apptainer", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Err(err).Str("name", name).Str("output", string(out)).Msg("apptainer instance start failed")
		return false, ""
	}

	s.mu.Lock()
	s.workdirs[name] = workdir
	s.mu.Unlock()

	log.Debug().Str("name", name).Msg("apptainer instance started")
	return true, name
}

func (s *ApptainerService) CleanContainer(id string) {
	s.mu.Lock()
	delete(s.workdirs, id)
	s.mu.Unlock()

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

	s.mu.Lock()
	workdir := s.workdirs[id]
	s.mu.Unlock()

	// Scope units only accept cgroup/timeout properties; sandboxing options like
	// LimitMEMLOCK, NoNewPrivileges, ProtectSystem are service-unit-only and are
	// rejected here. Apptainer rootless mode already applies PR_SET_NO_NEW_PRIVS
	// via user namespaces; the container also runs as non-root SubmitUid.
	runArgs := []string{
		"--scope",
		"--quiet",
		fmt.Sprintf("--property=RuntimeMaxSec=%d", timeout),
		"apptainer", "exec",
	}

	if workdir != "" {
		runArgs = append(runArgs, "--pwd", workdir)
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

	return exitCode, outBuf.String() + "\n" + errBuf.String(), nil
}

func (s *ApptainerService) GetContainerLogs(id string) (string, error) {
	// Apptainer instances don't have built-in log collector; output is captured per-step in ExecContainer.
	return "", nil
}
