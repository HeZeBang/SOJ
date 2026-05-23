# SOJ — Secure Online Judge

A small SSH-fronted online judge that runs each submission inside an
[Apptainer](https://apptainer.org/) container, with per-step `systemd-run` scopes
for timeout enforcement. Users connect over SSH to submit, list, and inspect
their submissions; a separate HTTP API exposes rank and submission data.

This document walks through a single-machine setup using a local SIF image and
a shell-based demo problem, plus the configuration knobs for sandbox hardening:
read-only rootfs, path masking, per-workflow capability dropping, and a
default-on seccomp filter that blocks known LPE syscall surfaces (io_uring,
AF_ALG, `unshare(CLONE_NEWUSER)`, keyctl, bpf, perf_event_open, vmsplice,
module load, etc.). See **[Sandbox security model](#sandbox-security-model)**
for the full picture.

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
- `apptainer` (tested with 1.5.0) — **must be built with libseccomp support**
  (`apptainer --version` should mention seccomp). Without it, `--security
  seccomp:…` is silently a no-op and the default profile won't be applied.
- `setpriv` (util-linux ≥ 2.34) **must be present inside every judge image**
  — it's how each non-privileged step drops uid inside the container.
  Debian, Alpine `apk add util-linux`, and Arch base images ship it. **busybox
  and minimal / distroless / scratch images do NOT** — busybox has a `setpriv`
  applet but it's a subset that lacks `--reuid` / `--regid` / `--clear-groups`
  and will fail with `setpriv: unrecognized option '--reuid=…'`. (The SFTP
  subsystem image is exempt — it self-drops in Go via
  `SOJ_DROP_UID`/`SOJ_DROP_GID` env vars, so busybox is fine there.)
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

The SFTP subsystem is a Go binary served as an SSH subsystem. It runs as
container root and **drops its own uid in-process** to
`$SOJ_DROP_UID`/`$SOJ_DROP_GID` (passed by SOJ) so uploaded files end up
owned by `SubmitUid` on the host. This means the SFTP image can stay on
busybox — it does not need an external `setpriv`.

> **Rebuild required after upgrading SOJ to the security-envelope refactor.**
> Old `/tmp/soj-sftp` binaries don't know about `SOJ_DROP_UID`/`SOJ_DROP_GID`
> and will keep serving as container root — uploaded files will be root-owned.

Build statically and wrap in a SIF:

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

### 6. Install the default seccomp profile

The repo ships an OCI seccomp profile at `seccomp/soj-default.json` that
blocks io_uring, AF_ALG sockets, `unshare(CLONE_NEWUSER)`, `keyctl`, `bpf`,
`perf_event_open`, kernel module / kexec syscalls, `mount` family, swap,
quotactl, and `vmsplice`. It's enabled by default via `DefaultSeccomp` (see
step 7) — install it once:

```bash
sudo mkdir -p /var/lib/soj/seccomp
sudo install -m 0644 seccomp/soj-default.json /var/lib/soj/seccomp/
```

You can point `DefaultSeccomp` elsewhere if you want to ship a stricter or
looser policy; see the [Sandbox security model](#sandbox-security-model)
section for the full list of denied syscalls and how to override per workflow.

### 7. Pre-pull the judge base image

The demo problem uses Debian. Pull once so the first submission isn't slow:

```bash
apptainer pull /tmp/debian.sif docker://debian:latest
```

### 8. Write `config.yaml`

Place `config.yaml` next to the `soj` binary. Fill in your own SSH pubkey
(the one you'll ssh with) and the judge UID/GID from step 1:

```yaml
HostKey: |
  -----BEGIN OPENSSH PRIVATE KEY-----
  ...contents of /tmp/soj_host_key...
  -----END OPENSSH PRIVATE KEY-----

ListenAddr: "0.0.0.0:2222"
APIAddr:    "0.0.0.0:8080"

# SSH authentication — pick one mode (see "SSH authentication" section below):
#
# Mode 1: Single key (original behavior)
# AllowedSSHPubkey: "ssh-ed25519 AAAA... user@host"
#
# Mode 2: GitHub key list — fetch each user's keys from github.com/<user>.keys
# Auth:
#   Mode: github-list
#   GitHubUsers:
#     - "alice"
#     - "bob"
#   GitHubToken: "ghp_xxx"           # optional, avoids rate limit
#   GitHubEndpoint: "https://github.com"  # optional, for GitHub Enterprise
#   KeyCachePath: "./keys_cache.json"     # optional, key cache file (default ./keys_cache.json)

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

# Security envelope defaults — applied to every workflow unless overridden.
# See "Sandbox security model" for the full table.
DefaultNoPrivs: true                               # apptainer --no-privs
DefaultDropCaps: []                                # extra caps to drop (uppercase)
DefaultAddCaps: []                                 # extra caps to add back (additive)
DefaultSeccomp: /var/lib/soj/seccomp/soj-default.json   # "" disables
```

### 9. Write the demo problem

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

### 10. Start SOJ

`soj` needs to `chown` per-submission directories into the judge UID, so it
runs as root:

```bash
sudo ./soj
```

### 11. Upload via SFTP and submit

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

## SSH authentication

SOJ supports two authentication modes, selected via `config.yaml`. In both
modes, the SSH username the client presents is used as the SOJ user identity.

### Mode 1: Single key (`AllowedSSHPubkey`)

The original mode. Only one public key is accepted; `AllowedSSHPubkey` left
empty accepts any key (no username verification).

```yaml
AllowedSSHPubkey: "ssh-ed25519 AAAA... user@host"
```

This is suitable for single-user setups or testing. **There is no protection
against username spoofing** — any client that presents the matching key can
claim any username.

### Mode 2: GitHub key list (`Auth.Mode: github-list`)

SOJ fetches each user's public keys from `https://github.com/<username>.keys`
at startup. The SSH username **must match** the GitHub username — the client's
presented key is looked up in the fetched set, and the owning GitHub username
must equal `ctx.User()`.

```yaml
Auth:
  Mode: github-list
  GitHubUsers:
    - "alice"
    - "bob"
    - "charlie"
  GitHubToken: "ghp_xxx"           # optional; avoids 60 req/hr rate limit
  GitHubEndpoint: "https://github.com"  # optional; for GitHub Enterprise
```

**How it works:**

1. At startup, SOJ checks for a cached key file (default `./keys_cache.json`,
   configurable via `Auth.KeyCachePath`). If the cache exists and is less than
   24 hours old, keys are loaded from disk — no GitHub fetch occurs.
2. If the cache is missing or stale, SOJ concurrently fetches keys for all
   listed users (with a progress bar). Failed fetches are logged as errors but
   do **not** prevent SOJ from starting — those users simply can't log in until
   keys are loaded.
3. Each key is mapped to its GitHub username by SHA256 fingerprint.
4. On SSH connect, the client's key fingerprint is looked up. If found and the
   owning username matches `ctx.User()`, authentication succeeds.
5. Admins can run `ssh oj adm refresh-keys` to re-fetch all keys without
   restarting SOJ (also updates the cache).

**Requirements:**

- SOJ host must be able to reach `github.com` (or the configured endpoint)
  over HTTPS.
- Users must have at least one SSH public key configured in their GitHub
  account settings (`Settings → SSH and GPG keys`).
- The SSH username used to connect to SOJ **must be identical** to the GitHub
  username.

**Backward compatibility:** If `Auth` is omitted but `AllowedSSHPubkey` is
set, SOJ falls back to single-key mode automatically. If neither is set, SOJ
accepts any key (open mode) with a warning.

---

## Configuration reference

### Global config (`config.yaml`)

| Field | Purpose |
|---|---|
| `HostKey` | SSH host private key (PEM) |
| `ListenAddr` / `APIAddr` | SSH and HTTP listen addresses |
| `AllowedSSHPubkey` | Single SSH pubkey allowed in; empty = accept any. Legacy; prefer `Auth` |
| `Auth.Mode` | Authentication mode: `single`, `github-list`, or empty (auto-detect) |
| `Auth.AllowedSSHPubkey` | Same as top-level `AllowedSSHPubkey` when `Auth.Mode=single` |
| `Auth.GitHubUsers` | List of GitHub usernames whose SSH keys are fetched at startup (`github-list` mode) |
| `Auth.GitHubToken` | Optional GitHub personal access token to raise rate limits (60→5000 req/hr) |
| `Auth.GitHubEndpoint` | GitHub base URL (default `https://github.com`); set for GitHub Enterprise |
| `Auth.KeyCachePath` | Path to SSH key cache file (default `./keys_cache.json`). Keys are cached for 24h to avoid GitHub fetches on restart |
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
| `DefaultProperties` | `systemd-run --property=` strings applied to every step when the workflow doesn't set its own `properties:` (cgroup/timeout-shaped values only) |
| `DefaultNoPrivs` | If true, every workflow runs with `apptainer --no-privs` (drop all caps + set NoNewPrivs). Recommended **true**. See [security model](#sandbox-security-model). |
| `DefaultDropCaps` | Extra `--drop-caps` applied to every workflow when the workflow doesn't set its own `dropcaps:`. Combine with `DefaultNoPrivs: false` to drop a narrow list. |
| `DefaultAddCaps` | `--add-caps` applied to every workflow. **Additive**: workflow `addcaps:` is appended on top, not a replacement. Keep this `[]` unless every workflow truly needs the same cap. |
| `DefaultSeccomp` | Host path to an OCI seccomp JSON applied to every workflow. `""` disables. Recommended: ship `seccomp/soj-default.json` to `/var/lib/soj/seccomp/`. |

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
    disablenetwork: true
    networkhostmode: false  # ignored when disablenetwork is true
    show: [1, 2]            # 1-indexed step numbers whose output streams to the user
    mounts:                 # extra bind mounts on top of /submits, /work, /result
      - type: bind
        source: /host/path
        target: /container/path
        readonly: true
    properties: []          # systemd-run --property=… overrides; see DefaultProperties
    mask: true              # see masking section below
    maskfiles: []           # optional override (empty = use DefaultMaskFiles)
    maskdirs: []

    # --- security envelope (see "Sandbox security model" for the full table) ---
    user: ""                # "" = SubmitUid; "root"/"0" = container root; "<n>" = numeric uid (n>0)
    privilegedsteps: []     # 1-indexed step numbers that skip the setpriv uid-drop
    noprivs: true           # apptainer --no-privs (overrides DefaultNoPrivs upward only)
    keepprivs: false        # apptainer --keep-privs; mutually exclusive with noprivs
    dropcaps: []            # nil ⇒ fall back to DefaultDropCaps; explicit [] ⇒ drop nothing
    addcaps: []             # ADDITIVE on top of DefaultAddCaps
    seccomp: ""             # "" ⇒ DefaultSeccomp; explicit path overrides
    noseccomp: false        # set true to disable the default profile entirely for this workflow
```

> **Migration**: the old `root: true` field is gone. Replace with `user: "root"` (or
> equivalently `user: "0"`). Unknown YAML fields are silently ignored by
> `yaml.v3`, so an unmigrated workflow will quietly downgrade to running as
> `SubmitUid`.

The final step (in the last workflow) must leave a `result.json` in `/result/`:

```json
{
  "success": true,
  "score": 100,
  "message": "...",
  "memory": 0,
  "time": 0,
  "tag": "6.00x"
}
```

The `tag` field is an optional string displayed alongside the score in the
rank table and other views (e.g. `90.00 (6.00x)`). Empty or omitted means no
tag is shown.

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
| In-container uid drop | `setpriv --reuid=<uid> --regid=<gid> --clear-groups --` wraps every non-privileged step |
| Auto-added caps | `CAP_SETUID` + `CAP_SETGID` injected into `--add-caps` whenever a workflow has any non-privileged step with `uid > 0` — required for the setpriv uid drop to succeed |

---

## Sandbox security model

### Threat model

A SOJ submission is *untrusted user code* running as root-by-default inside an
Apptainer container. Without the layers below, the kernel attack surface
exposed to that code includes io_uring, AF_ALG crypto sockets,
`unshare(CLONE_NEWUSER)`, keyrings, `bpf(2)`, `perf_event_open(2)`, kernel
module loading, kexec, and the `mount` / `swap` / `quotactl` family — all
historically reachable LPE primitives.

SOJ's defense is three concentric layers, plus a host-level prerequisite:

1. **Kernel-level filter (seccomp)** — `--security seccomp:<profile>` at
   `apptainer instance start`. Default profile is `seccomp/soj-default.json`,
   denying the syscalls above. Applies to every process the container ever
   spawns, including via `apptainer exec`.
2. **Capability bound (apptainer `--no-privs` + `--add-caps`)** — drops all
   Linux capabilities at instance start, then adds back only what the
   workflow declares plus what setpriv needs.
3. **In-container uid drop (`setpriv`)** — each non-privileged step is wrapped
   in `setpriv --reuid=<uid> --regid=<gid> --clear-groups --` so the user
   workload runs as `SubmitUid`, not container-root, **with zero capabilities
   in its effective/permitted/inheritable sets** (uid 0→non-0 transition
   wipes those by default).
4. **Host-level closure** — seccomp cannot inspect pointer arguments, so
   `clone3(CLONE_NEWUSER)` is **not filterable** at our layer (see the
   seccomp profile section for the long story). The host must set
   `kernel.unprivileged_userns_clone = 0` to close that gap; see
   [Recommended host-level hardening](#recommended-host-level-hardening-operator)
   for the full sysctl + modprobe list.

### Execution model — what actually happens per step

```
host (root)
  └─> apptainer instance start
        --no-privs                    ← drop caps + NoNewPrivs
        --add-caps CAP_SETUID,CAP_SETGID,<workflow.addcaps>
        --security seccomp:/path
        … bind mounts / mask binds / image / instance name

  └─> for each step:
        systemd-run --scope --property=RuntimeMaxSec=<timeout> ...
          apptainer exec --pwd /work instance://<name>
            [setpriv --reuid=<uid> --regid=<gid> --clear-groups --]   # skipped if privileged
              sh -c "<step command>"
```

**SFTP subsystem exception.** The SFTP container takes a shortcut: the
`/soj-sftp` Go binary itself calls `setresuid`/`setresgid` from the
`SOJ_DROP_UID`/`SOJ_DROP_GID` env vars before serving any request, so SOJ
launches it as container-root (`Privileged: true`) and the in-container
setpriv step is skipped entirely. This lets the SFTP SIF stay on `busybox`
even though busybox's setpriv applet is too limited for the judge path.
CAP_SETUID/CAP_SETGID are still added at instance start so the in-Go drop
syscall succeeds; they are wiped at the uid transition the same way they
would be for the judge path.

Important consequence: **`addcaps` does not survive the setpriv uid drop**.
A workflow with `noprivs: true, addcaps: [CAP_SYS_NICE]` and non-privileged
steps gives the *step command* zero effective capabilities — the cap was
present only during the brief root window before setpriv ran. If the workload
genuinely needs CAP_SYS_NICE, either (a) mark the step `privilegedsteps:` to
skip setpriv (it then runs as container-root with the cap), or (b) rely on
cgroup constraints (`AllowedCPUs=`, `AllowedMemoryNodes=`) which bind the
default-allow set for `sched_setaffinity` / `mbind` without needing the cap.

### Default seccomp profile (`seccomp/soj-default.json`)

`defaultAction: SCMP_ACT_ALLOW` with `SCMP_ACT_ERRNO` rules on:

| Group | Syscalls |
|---|---|
| io_uring | `io_uring_setup` (425), `io_uring_enter` (426), `io_uring_register` (427) |
| Keyring | `add_key`, `request_key`, `keyctl` |
| Tracing | `bpf`, `perf_event_open` |
| Module / kexec | `init_module`, `finit_module`, `delete_module`, `kexec_load`, `kexec_file_load` |
| Mount family | `pivot_root`, `mount`, `umount`, `umount2`, `move_mount`, `open_tree`, `fsopen`, `fsmount`, `fsconfig`, `fspick`, `swapon`, `swapoff`, `quotactl`, `quotactl_fd`, `nfsservctl` |
| Page-cache LPE | `vmsplice` |
| User namespace | `unshare` / `clone` / `setns` when arg has `CLONE_NEWUSER` bit set |
| Crypto socket | `socket(AF_ALG, …)` (family=38) |
| Network bypass | `socket(AF_XDP, …)` (family=44) |

Explicitly allowed (not in any deny rule, picked up by the default-allow
action): `sched_setaffinity`, `sched_getaffinity`, `getcpu`, `set_mempolicy`,
`get_mempolicy`, `mbind`, `setresuid`, `setresgid`, `setgroups` — i.e.
everything `numactl` and `setpriv` need.

> **Why `clone3` is not filtered.** Earlier drafts of this profile returned
> ENOSYS for `clone3` so that glibc would fall back to `clone(2)` (which we
> filter on the `CLONE_NEWUSER` bit). glibc ≥ 2.37 removed that fallback and
> calls `clone3` unconditionally for `pthread_create`, so an `ENOSYS` rule
> breaks the apptainer starter binary on any recent host (Manjaro, Arch,
> Fedora 38+, Debian trixie). `clone3`'s flag argument lives in a pointed-to
> `clone_args` struct, which seccomp cannot inspect — so the only useful
> options are "allow" or "deny outright"; allow is what we ship. To close
> the resulting user-namespace gap, apply the host sysctl listed below.

The profile is JSON in OCI runtime-spec format; you can edit
`/var/lib/soj/seccomp/soj-default.json` directly or point `DefaultSeccomp` at
a different file.

### Recommended host-level hardening (operator)

These sysctls and module blacklists complement the per-container envelope and
close gaps that seccomp cannot reach from inside the container. Apply them on
the SOJ host:

```bash
# /etc/sysctl.d/99-soj-hardening.conf
kernel.unprivileged_userns_clone = 0   # blocks unprivileged userns creation
                                        # even via clone3(CLONE_NEWUSER) which
                                        # we cannot filter at seccomp's layer
kernel.io_uring_disabled         = 2   # belt + suspenders for the io_uring
                                        # syscall filter (the kernel build
                                        # may have CONFIG_IO_URING=y)
kernel.kptr_restrict             = 2
kernel.dmesg_restrict            = 1
kernel.unprivileged_bpf_disabled = 1
```

```bash
# /etc/modprobe.d/soj-hardening.conf — block crypto-socket attack surface
# even if CAP_SYS_MODULE is somehow reachable
install af_alg          /bin/false
install algif_hash      /bin/false
install algif_skcipher  /bin/false
install algif_aead      /bin/false
install algif_rng       /bin/false
install xfrm_user       /bin/false
```

Apply with `sudo sysctl --system` and `sudo update-initramfs -u` (or your
distro's equivalent), then reboot. These are out of SOJ's reach — the
sandbox can only filter what the kernel is willing to filter.

### Per-workflow field reference

Each field below independently overrides (or augments) a default. The
"interaction with default" column says what `""` / `nil` / `false` means in
that field.

| Field | Type | Meaning | Interaction with default |
|---|---|---|---|
| `user` | string | Effective uid inside the container for non-privileged steps. `""` ⇒ `SubmitUid`; `"root"` or `"0"` ⇒ uid 0 (skips setpriv globally); `"<n>"` ⇒ numeric uid (must be `>0`). | No global default; always falls back to `SubmitUid`. |
| `privilegedsteps` | `[]int` (1-indexed) | Steps in this list skip the setpriv wrapper, running as container-root. Useful for prep/cleanup that must touch root-owned paths. | No default. |
| `noprivs` | bool | Adds `--no-privs` to instance start (drop all caps + NoNewPrivs). | OR'd with `DefaultNoPrivs` — workflow can only add, not remove. To remove, use `keepprivs: true`. |
| `keepprivs` | bool | Adds `--keep-privs`, retaining root's full capability set. Use only for trusted infrastructure workflows. | Mutually exclusive with `noprivs`; if both end up true, behavior is apptainer-version-dependent — avoid. |
| `dropcaps` | `[]string` | `apptainer --drop-caps CAP_FOO,CAP_BAR,…`. Cap names are case-insensitive, with or without the `CAP_` prefix. | `nil` ⇒ fall back to `DefaultDropCaps`. Explicit `[]` ⇒ drop nothing extra. |
| `addcaps` | `[]string` | `apptainer --add-caps …`. Caps survive only for *privileged* steps; non-privileged steps lose them at the setpriv uid drop (see above). | **Additive on top of `DefaultAddCaps`** — not a replacement. |
| `seccomp` | string | Host path to OCI seccomp JSON. | `""` ⇒ fall back to `DefaultSeccomp` unless `noseccomp: true`. |
| `noseccomp` | bool | Disables the default seccomp profile entirely for this workflow (does not affect an explicit `seccomp:` path). | Default false. |

### Recommended recipes

**Locked-down student code (default):** rely on platform defaults — students
just write `workflow:` entries with no security knobs.

```yaml
workflow:
  - image: …
    timeout: …
    mask: true
    # nothing else — picks up DefaultNoPrivs + DefaultSeccomp + SubmitUid
```

**Numactl / NUMA pinning workload (proj3 style):** lock everything down, let
the cgroup bound numactl rather than handing it CAP_SYS_NICE.

```yaml
workflow:
  - image: …
    noprivs: true                    # belt: drop all caps
    properties:                      # suspenders: cgroup pins the affinity set
      - "AllowedCPUs=0-7"
      - "AllowedMemoryNodes=0"
      - "MemoryMax=4G"
      - "CPUQuota=800%"
    # no addcaps: CAP_SYS_NICE wouldn't survive setpriv anyway; cgroup is
    # what actually keeps the binding inside the allowed CPU/node set.
```

**Trusted post-processing as root (judge writer):** stage / build runs as
`SubmitUid`, then the trusted scorer step runs as container-root.

```yaml
workflow:
  - image: …
    privilegedsteps: [3]   # step 3 (1-indexed) skips setpriv
    steps:
      - "cp /submits/*.c /work/"                           # uid 942
      - "cd /work && make"                                 # uid 942
      - "/scaffold/judge.sh /work/output /result/result.json"  # uid 0
```

**Per-problem cap addition (privileged step needs it):**

```yaml
workflow:
  - image: …
    privilegedsteps: [4]
    addcaps: [CAP_SYS_PTRACE]   # step 4 (privileged, no setpriv) keeps this cap
    # non-privileged steps still get the cap injected at instance start, but
    # lose it at the setpriv uid drop. It's effectively step-4-only.
```

**Per-problem seccomp override (e.g. a problem that needs `unshare`):**

```yaml
workflow:
  - image: …
    seccomp: /var/lib/soj/seccomp/permissive-unshare.json
    # or, to disable seccomp entirely for one workflow:
    # noseccomp: true
```

### Why `addcaps` does not "just work"

The Linux capability model wipes a process's permitted / effective /
inheritable sets when it transitions from uid 0 to a non-zero uid (unless
`SECBIT_KEEP_CAPS` or `--ambient-caps` is set). SOJ's setpriv invocation does
neither, so capabilities added at apptainer instance start are visible to the
*pre-setpriv* root shim only. If a future change wires `setpriv
--ambient-caps`, AddCaps will carry through to the workload — until then,
treat AddCaps as a privileged-step-only mechanism.

---

## SSH commands

Once SOJ is running, `ssh -p 2222 <user>@<host>` opens an interactive session.
Common commands:

| Command | Aliases | Purpose |
|---|---|---|
| `submit <problem_id>` | `sub` | Submit and run the judge workflow |
| `list [page]` | `ls` | List your submissions |
| `status <submit_id>` | `st` | Show one submission in detail |
| `describe [problem_id]` | `desc` | With no arg: list all problem IDs. With one: show id, text, and required submits |
| `my` | | Your best scores per problem |
| `rank` | `rk` | Leaderboard; scores show `(tag)` suffix when available (e.g. `90.00 (6.00x)`) |
| `token` | | Token cookie for the HTTP API |

SFTP is also exposed as a subsystem (`sftp -P 2222 <user>@<host>`) and lands the
user in `/work` inside the SFTP container, which is `SubmitsDir/<user>` on the
host.

Admin-only commands (prefix `adm`):

| Command | Purpose |
|---|---|
| `adm list [page]` | List all submissions from all users |
| `adm status <submit_id>` | View any submission by ID |
| `adm pause` | Pause new submissions |
| `adm refresh-keys` | Re-fetch SSH keys from GitHub (`github-list` mode only) |

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

# Remove the masked-paths source dir and the installed seccomp profile
sudo rm -rf /var/lib/soj

# Remove pre-built images and keys
rm -f /tmp/soj-sftp.sif /tmp/soj-sftp /tmp/soj-sftp.def
rm -f /tmp/debian.sif
rm -f /tmp/soj_host_key /tmp/soj_host_key.pub
rm -f keys_cache.json

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
| `exec: setpriv: not found` or step exits 127 immediately | Judge image lacks `setpriv` (util-linux). Rebuild the image with `util-linux` installed, or mark the affected step `privilegedsteps: [<n>]` to skip setpriv |
| `setpriv: unrecognized option '--reuid=…'` (BusyBox usage banner) | Image's `setpriv` is busybox's applet, not util-linux. Switch the base to Alpine + `apk add util-linux`, or Debian, or any image that ships full util-linux. SFTP is exempt and self-drops in Go |
| `setpriv: setresuid: Operation not permitted` | `noprivs: true` was applied but `CAP_SETUID/CAP_SETGID` are missing from the bounding set. The evaluator auto-adds them — if you see this, check that the workflow's `dropcaps:` isn't dropping them again, and that no host-level seccomp or AppArmor profile is interfering |
| `setresgid(N): operation not permitted` (in `/soj-sftp` output) | SFTP container started without `CAP_SETUID/CAP_SETGID` in its bounding set. This shouldn't happen — `file_transfer/sftp.go` adds them when `SubmitUid > 0`. Confirm SOJ was rebuilt after the security-envelope refactor (`go build`) |
| SFTP uploads land as root-owned on host | `/soj-sftp` binary is old (pre security-envelope refactor) and doesn't read `SOJ_DROP_UID`. Rebuild `subsystems/sftp` and the `.sif` per step 5 |
| `seccomp profile applied but workload still uses io_uring` | `apptainer --version` doesn't mention seccomp — apptainer was built without libseccomp and `--security seccomp:` is silently ignored. Rebuild apptainer with seccomp support |
| `apptainer instance start failed: invalid argument: --security seccomp:…` | `DefaultSeccomp` points at a missing or malformed JSON. Validate with `python3 -m json.tool /var/lib/soj/seccomp/soj-default.json` |
| `runtime/cgo: pthread_create failed: Operation not permitted` then segfault | The seccomp profile is filtering `clone3` (e.g. returning ENOSYS). glibc ≥ 2.37 no longer falls back to `clone(2)` and the apptainer starter dies. Remove any `clone3` rule from the profile; rely on `kernel.unprivileged_userns_clone = 0` at the host level instead |
| Old workflow `root: true` is silently ignored | The field was removed; use `user: "root"` (or `user: "0"`). yaml.v3 doesn't error on unknown fields |

---

## Acknowledgement

This project is based on [ZJUSCT/SOJ](https://github.com/ZJUSCT/SOJ)