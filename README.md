# SOJ — Secure Online Judge

A small SSH-fronted online judge that runs each submission inside an
[Apptainer](https://apptainer.org/) container, with per-step `systemd-run` scopes
for timeout enforcement. Users connect over SSH to submit, list, and inspect
their submissions; a separate HTTP API exposes rank and submission data.

This document walks through a single-machine setup using a local SIF image and
a shell-based demo problem, plus the configuration knobs introduced for sandbox
hardening (read-only rootfs, configurable path masking).

---

## Architecture in one breath

```
ssh client ──► SSH server (gliderlabs/ssh)
                 │
                 ├── submit <id>    ──► Evaluator ──► apptainer instance start
                 │                                    │
                 │                                    └─ each step:
                 │                                        systemd-run --scope --property=RuntimeMaxSec=N
                 │                                          apptainer exec --pwd /work instance://...
                 │
                 └── sftp           ──► SFTP subsystem container (stdio proxied)

HTTP API (gin) ─► SQLite (gorm) — users, submissions, scores
```

Per-submission layout on the host:

```
SubmitWorkDir/<submitID>/
  ├── submits/   (bind-mounted into container as /submits, read-only)
  ├── work/      (bind-mounted as /work, read-write)
  └── result/    (bind-mounted as /result, read-write; result.json read after run)
```

---

## Prerequisites

- Linux with `systemd` running (scope units are used for timeouts)
- `apptainer` (tested with 1.5.0) — must be runnable rootless
- `go` 1.21+ for building
- `docker` only needed if you want to build subsystem images via Dockerfile;
  otherwise plain `go build` + an Apptainer def file is enough

---

## Setup walkthrough

The commands below reproduce the bring-up that was tested end-to-end.

### 1. Create the judge user

```bash
sudo useradd -r -m -s /sbin/nologin judge
JUDGE_UID=$(id -u judge); JUDGE_GID=$(id -g judge)
echo "judge UID=$JUDGE_UID GID=$JUDGE_GID"
```

### 2. Create runtime directories

```bash
sudo mkdir -p /data/soj/{submits,work,problems}
sudo chown -R judge:judge /data/soj
```

### 3. Generate the SSH host key

```bash
ssh-keygen -t ed25519 -f /tmp/soj_host_key -N "" -C "soj-host"
```

You will paste the contents of `/tmp/soj_host_key` into `config.yaml` (see step 6).

### 4. Build the SOJ binary

```bash
cd /path/to/SOJ
go build -o soj .
```

### 5. Build the SFTP subsystem SIF

The SFTP subsystem is a Go binary served as an SSH subsystem.
Build it statically and wrap it in a SIF using `apptainer build --fakeroot`.

```bash
cd subsystems/sftp
CGO_ENABLED=0 GOOS=linux go build -o /tmp/soj-sftp .

cat > /tmp/soj-sftp.def <<'EOF'
Bootstrap: docker
From: busybox

%files
    /tmp/soj-sftp /soj-sftp

%runscript
    exec /soj-sftp "$@"
EOF

apptainer build --fakeroot /tmp/soj-sftp.sif /tmp/soj-sftp.def
```

### 6. Pre-pull the judge base image

The demo problem uses Debian. Pull once so the first submission isn't slow:

```bash
apptainer pull /tmp/debian.sif docker://debian:latest
```

### 7. Write `config.yaml`

Place `config.yaml` next to the `soj` binary. Fill in your own SSH pubkey
(the one you'll ssh with) and the judge UID/GID from step 1:

```yaml
HostKey: |
  -----BEGIN OPENSSH PRIVATE KEY-----
  ...contents of /tmp/soj_host_key...
  -----END OPENSSH PRIVATE KEY-----

ListenAddr: "0.0.0.0:2222"
APIAddr:    "0.0.0.0:8080"

# Public key allowed to log in. If empty, any key is accepted.
AllowedSSHPubkey: "ssh-ed25519 AAAA... user@host"

SubmitsDir:        /data/soj/submits
SubmitWorkDir:     /data/soj/work
RealSubmitsDir:    /data/soj/submits
RealSubmitWorkDir: /data/soj/work
ProblemsDir:       /data/soj/problems

SqlitePath: /data/soj/soj.db
SftpImage:  /tmp/soj-sftp.sif

SubmitUid: 942   # from $JUDGE_UID
SubmitGid: 941   # from $JUDGE_GID

Admins:
  - your-ssh-username

# Sensitive paths to mask when a workflow opts in via mask: true.
# Files are overlaid with /dev/null; dirs are overlaid with an empty dir.
DefaultMaskFiles:
  - /proc/cmdline
  - /proc/kallsyms
  - /proc/config.gz
  - /proc/sysrq-trigger
  - /proc/kcore
  - /proc/keys
DefaultMaskDirs:
  - /proc/sys
  - /proc/fs
  - /proc/bus
  - /proc/irq
  - /proc/scsi
  - /sys/firmware
  - /sys/kernel/debug
```

### 8. Write the demo problem

`/data/soj/problems/hello.yaml`:

```yaml
version: 1
id: hello
text: "Submit answer.txt containing the number 42"
submits:
  - path: answer.txt
    isdir: false
workflow:
  - image: /tmp/debian.sif
    steps:
      - >-
        ans=$(cat /submits/answer.txt | tr -d '[:space:]');
        if [ "$ans" = "42" ]; then
          printf '{"success":true,"score":100,"message":"Correct! Got %s"}' "$ans" > /result/result.json;
        else
          printf '{"success":true,"score":0,"message":"Wrong answer: got %s"}' "$ans" > /result/result.json;
        fi
    timeout: 15
    disablenetwork: true
    show: [1]
    mask: true
```

### 9. Start SOJ

`soj` needs to `chown` per-submission directories into the judge UID, so it
runs as root:

```bash
sudo ./soj
```

### 10. Upload via SFTP and submit

In another terminal, drive the same path real users do: upload the file
through the SFTP subsystem with `scp`, then trigger the judge with
`ssh ... submit`. Remote paths are relative to `/work` inside the SFTP
container, which is `SubmitsDir/<user>` on the host, so `hello/answer.txt`
lands at `/data/soj/submits/zambar/hello/answer.txt`.

```bash
echo "42" > /tmp/answer.txt

# Upload through the SFTP subsystem container
scp -P 2222 /tmp/answer.txt zambar@localhost:hello/answer.txt

# Run the judge
ssh -p 2222 zambar@localhost submit hello
```

Expected output ends with:

```
Score 100.00 max.100 (Unweighted)
Judgement Message:
    Correct! Got 42
```

Try `echo 99 > /tmp/answer.txt && scp -P 2222 /tmp/answer.txt zambar@localhost:hello/answer.txt`
followed by another `submit` to see the wrong-answer path.

> **scp on older OpenSSH**
> `scp` started using the SFTP protocol in OpenSSH 8.7 (opt-in) and switched
> by default in 9.0. On `8.7 ≤ ver < 9.0` add `-s`; on `< 8.7` use `sftp`
> directly instead of `scp`.

---

## Configuration reference

### Global config (`config.yaml`)

| Field | Purpose |
|---|---|
| `HostKey` | SSH host private key (PEM) |
| `ListenAddr` / `APIAddr` | SSH and HTTP listen addresses |
| `AllowedSSHPubkey` | Single SSH pubkey allowed in; empty = accept any |
| `SubmitsDir` | Where uploaded submissions live (`<dir>/<user>/<problem>/`) |
| `SubmitWorkDir` | Per-submission scratch dir (created and torn down per run) |
| `RealSubmitsDir` / `RealSubmitWorkDir` | Host paths exposed to the container as `SOJ_REAL_*` env vars. Same as the *Dir fields unless SOJ itself runs in a container |
| `ProblemsDir` | Directory of `*.yaml` problem definitions; filename minus `.yaml` is the problem id |
| `SqlitePath` | SQLite DB file |
| `SftpImage` | Apptainer image (SIF path or `docker://` URI) for the SFTP subsystem container |
| `SubmitUid` / `SubmitGid` | UNIX uid/gid that owns submission files and runs the container processes |
| `Admins` | Usernames with admin commands |
| `DefaultMaskFiles` | Paths masked with `/dev/null` when a workflow sets `mask: true` and doesn't override `maskfiles` |
| `DefaultMaskDirs` | Paths masked with an empty directory under the same conditions |

### Problem YAML

```yaml
version: 1
id: <string>
text: <description shown to users>
weight: 1.0                 # score multiplier; defaults to 1.0
submits:
  - path: <relative path under SubmitsDir/<user>/<problem>/>
    isdir: false            # set true to walk a directory tree
workflow:                   # one or more stages, run sequentially
  - image: <SIF path or docker://URI>
    steps:                  # shell commands; each runs in a fresh systemd scope
      - "..."
    timeout: 15             # per-step timeout in seconds
    root: false             # if true, exec as uid 0 inside the container
    disablenetwork: true
    networkhostmode: false  # ignored when disablenetwork is true
    show: [1, 2]            # 1-indexed step numbers whose output streams to the user
    privilegedsteps: []     # 1-indexed steps that run with elevated privileges
    mounts:                 # extra bind mounts on top of /submits, /work, /result
      - type: bind
        source: /host/path
        target: /container/path
        readonly: true
    mask: true              # see masking section below
    maskfiles: []           # optional override (empty = use DefaultMaskFiles)
    maskdirs: []
```

The final step (in the last workflow) must leave a `result.json` in `/result/`:

```json
{
  "success": true,
  "score": 100,
  "message": "...",
  "memory": 0,
  "time": 0
}
```

Environment variables available in every step:

```
SOJ_SUBMITS_DIR=/submits        SOJ_REAL_SUBMITDIR=<host path>
SOJ_WORK_DIR=/work              SOJ_REAL_WORKDIR=<host path>
SOJ_RESULT_DIR=/result          SOJ_REAL_RESULTDIR=<host path>
SOJ_PROBLEM=<problem id>        SOJ_SUBMIT=<submission id>
SOJ_WORK_UID=<SubmitUid>        SOJ_WORK_GID=<SubmitGid>
```

### Path masking

Apptainer's `--no-mount proc,sys` is all-or-nothing and breaks tools that need
`/proc/self/*`. SOJ instead bind-mounts harmless sources over specific
sensitive paths, leaving the rest of `/proc` and `/sys` usable.

How the per-workflow knobs combine with `DefaultMaskFiles` / `DefaultMaskDirs`:

| Workflow setting | Effective files / dirs masked |
|---|---|
| `mask: false` or unset | none |
| `mask: true` | `DefaultMaskFiles` + `DefaultMaskDirs` |
| `mask: true` + `maskfiles: [...]` | given files + `DefaultMaskDirs` |
| `mask: true` + `maskdirs: [...]` | `DefaultMaskFiles` + given dirs |
| `mask: true` + both set | given files + given dirs (defaults ignored) |

### Sandbox defaults baked into the evaluator

These are set in `judge/evaluator.go` and apply to every judge container; they
are not driven by YAML today:

| Behavior | How |
|---|---|
| Read-only rootfs | `--writable-tmpfs` is omitted; SIF layers are RO by default |
| `--containall` | Always passed |
| `--net --network=none` | When `disablenetwork: true` |
| Timeout per step | `systemd-run --scope --property=RuntimeMaxSec=<timeout>` |
| Username validation | Only `[A-Za-z0-9_-]{1,64}` allowed for SFTP sessions |

---

## SSH commands

Once SOJ is running, `ssh -p 2222 <user>@<host>` opens an interactive session.
Common commands:

| Command | Aliases | Purpose |
|---|---|---|
| `submit <problem_id>` | `sub` | Submit and run the judge workflow |
| `list [page]` | `ls` | List your submissions |
| `status <submit_id>` | `st` | Show one submission in detail |
| `my` | | Your best scores per problem |
| `rank` | `rk` | Leaderboard |
| `token` | | Token cookie for the HTTP API |

SFTP is also exposed as a subsystem (`sftp -P 2222 <user>@<host>`) and lands the
user in `/work` inside the SFTP container, which is `SubmitsDir/<user>` on the
host.

For the **end-user perspective** (how submitters actually interact with the
running deployment — upload conventions, OpenSSH version quirks, every command
with examples), see [`GUIDE.md`](./GUIDE.md). This README is the operator's
guide; `GUIDE.md` is what you hand to people who only need to submit problems.

---

## Teardown / cleanup

Stop SOJ, then remove the runtime state. The judge user and `/data/soj` survive
binary rebuilds, so you only need this when starting fresh or removing the
deployment entirely.

```bash
# Stop the SOJ process
sudo pkill -f './soj$'

# Stop any leftover apptainer instances
apptainer instance list
apptainer instance stop --all   # add `sudo` if any were started as root

# Remove all runtime data (DB, submissions, work dirs, problems)
sudo rm -rf /data/soj

# Remove the masked-paths source dir
sudo rm -rf /var/lib/soj

# Remove pre-built images and keys
rm -f /tmp/soj-sftp.sif /tmp/soj-sftp /tmp/soj-sftp.def
rm -f /tmp/debian.sif
rm -f /tmp/soj_host_key /tmp/soj_host_key.pub

# Drop the judge UNIX user (and its home dir)
sudo userdel -r judge

# Optional: clear the Apptainer image cache
apptainer cache clean --force
```

If SOJ was run as root, some files under `~/.apptainer` and `/var/tmp` may be
owned by root — clean those with `sudo`.

---

## Troubleshooting

| Symptom | Cause |
|---|---|
| `apptainer instance start failed: unknown flag: --pwd` | Old binary; rebuild after pulling latest |
| `Unknown assignment: LimitMEMLOCK=0` | systemd scope units reject service-only properties; rebuild after pulling latest |
| `WARNING: ... readlink /proc/self/exe: no such file or directory` | A workflow ran with `mask: true` and `/proc` fully blocked. Use the path-level mask defaults instead of `--no-mount proc` |
| `failed to read result file` | The last workflow step didn't write `/result/result.json` |
| `rejected sftp session: invalid username` | Username contains characters outside `[A-Za-z0-9_-]` — required to avoid argument injection into `apptainer` |
| Submissions stuck at `dead` | A previous SOJ run crashed mid-judge. Status is rewritten to `dead` at startup; submit again |

---

## Acknowledgement

This project is based on [ZJUSCT/SOJ](https://github.com/ZJUSCT/SOJ)