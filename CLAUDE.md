# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & run

```bash
go build -o soj .              # build the main binary
sudo ./soj                     # run; needs root because the evaluator chowns
                               # per-submission dirs into SubmitUid/SubmitGid
go build ./...                 # verify everything compiles
```

There are no tests in this repo. After editing any package, the only quick
correctness check is `go build ./...`.

The binary reads `config.yaml` from its current working directory at startup.

## Big-picture architecture

SOJ is a single Go process that fronts an SSH server (`gliderlabs/ssh`) and a
gin HTTP API, persists to SQLite via gorm, and runs each submission inside an
**Apptainer** container. It used to use Docker; that migration is recent and
some artifacts remain (e.g. `docker/docker` is still in `go.mod`, the
`Dockerfile` is for building SOJ itself).

The two important layers to understand:

### Sandbox layer (`file_transfer/apptainer.go`)

`ApptainerService` implements two-step container execution:

1. `RunImage` → `apptainer instance start <image> <name>`. Starts a long-lived
   instance. `--pwd` is **not** supported here; the working directory must be
   passed at exec time.
2. `ExecContainer` → `systemd-run --scope --property=RuntimeMaxSec=N apptainer exec --pwd <wd> instance://<name> sh -c <cmd>`.
   Each step gets its own scope so the timeout is enforced by systemd.

Key constraints that bite if you ignore them (we hit all of these):

- **Scope units only accept cgroup/timeout properties.** `LimitMEMLOCK`,
  `NoNewPrivileges`, `ProtectSystem`, `ProtectHome` are *service*-unit options
  and are rejected with `Unknown assignment: ...` when passed to a scope.
- **`--pwd` is `apptainer exec` only.** Passing it to `instance start` fails
  with `unknown flag: --pwd`. The service stores per-instance workdirs in a
  `map[string]string` and re-applies them on every exec.
- **`--writable-tmpfs` is the inverse of "readonly rootfs".** Omitting it
  leaves the SIF rootfs read-only, which is the desired default. The
  `ReadonlyRootfs` parameter only controls whether `--writable-tmpfs` is added.
- **`/proc` and `/sys` masking is per-path, not all-or-nothing.** Apptainer's
  `--no-mount proc,sys` removes them entirely and breaks tools that need
  `/proc/self/exe`. Instead, sensitive files are overlaid with `/dev/null` and
  sensitive dirs with an empty host directory (`emptyMaskDir` =
  `/var/lib/soj/empty-mask`, created at service init).
- **Argument injection via usernames.** `sess.User()` flows into apptainer
  instance names and into a filesystem path. `isValidUsername` in
  `file_transfer/sftp.go` rejects anything outside `[A-Za-z0-9_-]{1,64}`.

### Evaluator layer (`judge/evaluator.go`)

Per submission:

1. Create `SubmitWorkDir/<submitID>/{submits,work,result}`, chown to
   `SubmitUid:SubmitGid`.
2. Copy user files from `SubmitsDir/<user>/<problem>/` into `submits/`,
   permissions 0400 (read-only by uid).
3. For each `workflow` entry in the problem YAML:
   - Resolve mask config (`workflow.Mask`/`MaskFiles`/`MaskDirs` ∪
     `Config.DefaultMaskFiles`/`DefaultMaskDirs`).
   - `RunImage` → instance start with bind mounts `/submits` (ro), `/work`,
     `/result`, plus any user-specified mounts.
   - For each step, `ExecContainer` under a fresh systemd scope.
4. Read `/result/result.json` (written by the last workflow step). Parse into
   `JudgeResult`, update DB, recompute user totals.

The interface between evaluator and sandbox is `SandboxInterface` in
`judge/evaluator.go`. Any change to apptainer.go's `RunImage`/`ExecContainer`
signatures must update this interface and both call sites
(`evaluator.go` and `file_transfer/sftp.go`).

### SFTP subsystem (`subsystems/sftp/`)

The SFTP subsystem runs as a *separate binary* (`/soj-sftp`) inside its own
Apptainer container, talking to the SSH session over stdio. Build it
statically with `CGO_ENABLED=0 go build` and wrap it in a SIF (see README
"Setup walkthrough" step 5). The path to the SIF is `Config.SftpImage`; it
used to be hardcoded to `docker.io/mrhaoxx/soj-subsystem-sftp` (an image that
isn't actually on Docker Hub).

## Configuration model

- `types.Config` (YAML keys = field names verbatim, e.g. `SubmitUid`,
  `DefaultMaskFiles`) — loaded once at startup from `./config.yaml`.
- `types.Problem` / `types.Workflow` (YAML keys = lowercased field names, e.g.
  `image`, `maskfiles`) — one file per problem under `ProblemsDir`. Filename
  minus `.yaml` becomes the problem ID; `id:` inside the file is informational.

Mask resolution lives entirely in `RunJudge`:

```
if workflow.Mask {
    files = workflow.MaskFiles ?? cfg.DefaultMaskFiles
    dirs  = workflow.MaskDirs  ?? cfg.DefaultMaskDirs
}
```

`workflow.Mask: false` (the zero value) ⇒ no masking. To change defaults,
edit `config.yaml`; to override per-problem, set `maskfiles`/`maskdirs` in
the workflow YAML.

## Result contract

The last workflow step must write `/result/result.json` matching
`types.JudgeResult` (`{success, score, message, memory, time}`). The
`samplejudge` subsystem historically wrote to `/work/result.json`; this was
fixed when `/result` was split out as a separate bind mount. When porting any
new judge subsystem, target `/result/result.json` (the env var
`SOJ_RESULT_DIR=/result` is provided to every step).

## Documentation map

- `README.md` — operator/deployment guide (this is what we wrote together;
  start here for setup, config, cleanup).
- `GUIDE.md` — end-user guide (submitter perspective: `ssh oj submit ...`,
  `scp` upload conventions, OpenSSH version notes).
- `api.md` — bare-minimum workflow stage I/O contract (largely placeholder).
