package file_transfer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrhaoxx/SOJ/types"
	"github.com/rs/zerolog/log"
)

const emptyMaskDir = "/var/lib/soj/empty-mask"

// RunImageOpts 是 apptainer instance start 的参数集合。安全相关的开关（NoPrivs /
// KeepPrivs / DropCaps / AddCaps / Seccomp）只能在 instance start 时一次性生效，
// 之后所有 exec 共享同一个能力 / seccomp 信封。
type RunImageOpts struct {
	Name            string
	Hostname        string
	Image           string
	Workdir         string // 给 exec 时的 --pwd 用
	Mounts          []types.Mount
	MaskFiles       []string
	MaskDirs        []string
	ReadonlyRootfs  bool
	NetworkDisabled bool
	NetworkHosted   bool
	Timeout         int
	Env             []string

	NoPrivs   bool
	KeepPrivs bool
	DropCaps  []string
	AddCaps   []string
	Seccomp   string // 宿主机路径，"" 表示不应用
}

// ExecOpts 是 apptainer exec（在 systemd-run 范围内）的参数集合。
// 非 Privileged 且 UID != 0 时，命令外层会包一层
//   setpriv --reuid=UID --regid=GID --clear-groups --
// 在容器内丢身份；Privileged 步骤则直接以容器 root 运行。
type ExecOpts struct {
	Cmd        string
	Timeout    int
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	Env        []string
	Properties []string

	UID        int
	GID        int
	Privileged bool
}

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

func (s *ApptainerService) RunImage(opts RunImageOpts) (ok bool, id string) {
	args := []string{"instance", "start"}

	// Mask sensitive paths by bind-mounting harmless sources over them.
	// Files: /dev/null overlays the path with a character device.
	// Dirs: an empty host directory overlays the path, hiding everything inside.
	for _, f := range opts.MaskFiles {
		args = append(args, "--bind", "/dev/null:"+f+":ro")
	}
	for _, d := range opts.MaskDirs {
		args = append(args, "--bind", emptyMaskDir+":"+d+":ro")
	}

	args = append(args, "--containall")

	if !opts.ReadonlyRootfs {
		// SIF images are read-only by default; --writable-tmpfs adds an ephemeral overlay.
		args = append(args, "--writable-tmpfs")
	}

	if opts.NetworkDisabled {
		args = append(args, "--net", "--network=none")
	}

	if opts.Hostname != "" {
		args = append(args, "--hostname", opts.Hostname)
	}

	// --pwd is not supported by instance start; workdir is passed at exec time instead.

	for _, m := range opts.Mounts {
		bind := fmt.Sprintf("%s:%s", m.Source, m.Target)
		if m.ReadOnly {
			bind += ":ro"
		}
		args = append(args, "--bind", bind)
	}

	for _, e := range opts.Env {
		args = append(args, "--env", e)
	}

	// 安全信封：能力 / seccomp。NoPrivs 会先清空 cap + 设置 NoNewPrivs，AddCaps 在
	// 这之后追加回来。--keep-privs 用于明确放行的提权工作流。
	if opts.NoPrivs {
		args = append(args, "--no-privs")
	}
	if opts.KeepPrivs {
		args = append(args, "--keep-privs")
	}
	if len(opts.DropCaps) > 0 {
		args = append(args, "--drop-caps", strings.Join(opts.DropCaps, ","))
	}
	if len(opts.AddCaps) > 0 {
		args = append(args, "--add-caps", strings.Join(opts.AddCaps, ","))
	}
	if opts.Seccomp != "" {
		args = append(args, "--security", "seccomp:"+opts.Seccomp)
	}

	args = append(args, opts.Image, opts.Name)

	cmd := exec.Command("apptainer", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Err(err).Str("name", opts.Name).Str("output", string(out)).Msg("apptainer instance start failed")
		return false, ""
	}

	s.mu.Lock()
	s.workdirs[opts.Name] = opts.Workdir
	s.mu.Unlock()

	log.Debug().Str("name", opts.Name).Msg("apptainer instance started")
	return true, opts.Name
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

func (s *ApptainerService) ExecContainer(id string, opts ExecOpts) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(opts.Timeout)*time.Second)
	defer cancel()

	s.mu.Lock()
	workdir := s.workdirs[id]
	s.mu.Unlock()

	// Scope units only accept cgroup/timeout properties; sandboxing options like
	// LimitMEMLOCK, NoNewPrivileges, ProtectSystem are service-unit-only and are
	// rejected here. NoNewPrivs / 能力裁剪都已在 apptainer instance start 时通过
	// --no-privs / --drop-caps 设好，并由内核 bounding set 在 exec 之间保持。
	runArgs := []string{
		"--scope",
		"--quiet",
		fmt.Sprintf("--property=RuntimeMaxSec=%d", opts.Timeout),
	}

	// Caller-supplied cgroup properties (AllowedCPUs, MemoryMax, CPUQuota, ...).
	// systemd applies same-key properties in order so later entries override earlier ones;
	// platform RuntimeMaxSec is written first so caller-supplied ones can in principle
	// override it, but callers shouldn't (workflow.Timeout is the source of truth).
	for _, p := range opts.Properties {
		runArgs = append(runArgs, "--property="+p)
	}

	runArgs = append(runArgs, "apptainer", "exec")

	if workdir != "" {
		runArgs = append(runArgs, "--pwd", workdir)
	}

	for _, e := range opts.Env {
		runArgs = append(runArgs, "--env", e)
	}

	runArgs = append(runArgs, "instance://"+id)

	// 在容器内通过 setpriv 把当前 root 身份降到目标 uid/gid。
	// Privileged 步骤跳过这一层，从而以容器 root 运行（但依然受 NoPrivs/seccomp 约束）。
	// setpriv 需要容器内有 util-linux（Debian/Alpine util-linux 包均自带）。
	if !opts.Privileged && opts.UID != 0 {
		runArgs = append(runArgs,
			"setpriv",
			"--reuid="+strconv.Itoa(opts.UID),
			"--regid="+strconv.Itoa(opts.GID),
			"--clear-groups",
			"--",
		)
	}

	runArgs = append(runArgs, "sh", "-c", opts.Cmd)

	cmd := exec.CommandContext(ctx, "systemd-run", runArgs...)

	var outBuf bytes.Buffer
	var errBuf bytes.Buffer

	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
	}

	if opts.Stdout != nil {
		cmd.Stdout = io.MultiWriter(opts.Stdout, &outBuf)
	} else {
		cmd.Stdout = &outBuf
	}

	if opts.Stderr != nil {
		cmd.Stderr = io.MultiWriter(opts.Stderr, &errBuf)
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
