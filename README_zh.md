# SOJ — 安全在线评测系统

一个小型 SSH 前端在线评测系统，每次提交在 [Apptainer](https://apptainer.org/) 容器中运行，通过 per-step `systemd-run` scope 实现超时控制。用户通过 SSH 连接进行提交、查看和检查操作；独立的 HTTP API 提供排行榜和提交数据。

本文档介绍使用本地 SIF 镜像和基于 shell 的演示题目的单机部署流程，以及沙箱加固的配置选项：只读根文件系统、路径屏蔽、per-workflow 权限剥离，以及默认开启的 seccomp 过滤器（阻止已知的 LPE 系统调用面，如 io_uring、AF_ALG、`unshare(CLONE_NEWUSER)`、keyctl、bpf、perf_event_open、vmsplice、模块加载等）。完整信息请参见 **[沙箱安全模型](#沙箱安全模型)**。

---

## 架构概览

```
ssh client ──► SSH server (gliderlabs/ssh)
                 │
                 ├── submit <id>    ──► 评测器 ──► apptainer instance start
                 │                                    │
                 │                                    └─ 每一步:
                 │                                        systemd-run --scope --property=RuntimeMaxSec=N
                 │                                          apptainer exec --pwd /work instance://...
                 │
                 └── sftp           ──► SFTP 子系统容器 (stdio 代理)

HTTP API (gin) ─► SQLite (gorm) — 用户、提交、分数
```

宿主机上每次提交的目录布局：

```
SubmitWorkDir/<submitID>/
  ├── submits/   (bind 挂载到容器 /submits，只读)
  ├── work/      (bind 挂载到 /work，读写)
  └── result/    (bind 挂载到 /result，读写；运行结束后读取 result.json)
```

---

## 前置要求

- Linux 系统且运行 `systemd`（使用 scope 单元实现超时）
- `apptainer`（测试版本 1.5.0）— **必须启用 libseccomp 支持编译**
  （`apptainer --version` 输出应包含 seccomp）。否则 `--security seccomp:…` 会被静默忽略，默认配置不会生效。
- `setpriv`（util-linux ≥ 2.34）**必须存在于 SOJ 使用的每个容器镜像中** — 这是非特权步骤在容器内降低 uid 的方式。Debian、Alpine、Arch 基础镜像已自带；最小化 / 无发行版 / scratch 镜像**不包含**，会报错 `exec: setpriv: not found`。
- `go` 1.21+ 用于编译
- 仅在需要通过 Dockerfile 构建子系统镜像时才需要 `docker`；否则只需 `go build` + Apptainer def 文件即可

---

## 部署流程

以下命令复现了端到端测试的完整部署过程。

### 1. 创建评测用户

```bash
sudo useradd -r -m -s /sbin/nologin judge
JUDGE_UID=$(id -u judge); JUDGE_GID=$(id -g judge)
echo "judge UID=$JUDGE_UID GID=$JUDGE_GID"
```

### 2. 创建运行时目录

```bash
sudo mkdir -p /data/soj/{submits,work,problems}
sudo chown -R judge:judge /data/soj
```

### 3. 生成 SSH 主机密钥

```bash
ssh-keygen -t ed25519 -f /tmp/soj_host_key -N "" -C "soj-host"
```

将 `/tmp/soj_host_key` 的内容粘贴到 `config.yaml` 中（见第 6 步）。

### 4. 编译 SOJ 二进制文件

```bash
cd /path/to/SOJ
go build -o soj .
```

### 5. 构建 SFTP 子系统 SIF

SFTP 子系统是一个 Go 二进制文件，作为 SSH 子系统提供服务。使用 `apptainer build --fakeroot` 静态编译并打包为 SIF。

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

### 6. 安装默认 seccomp 配置

仓库在 `seccomp/soj-default.json` 提供了一个 OCI seccomp 配置，阻止 io_uring、AF_ALG 套接字、`unshare(CLONE_NEWUSER)`、`keyctl`、`bpf`、`perf_event_open`、内核模块 / kexec 系统调用、`mount` 系列、swap、quotactl 和 `vmsplice`。默认通过 `DefaultSeccomp` 启用（见第 7 步）— 安装一次即可：

```bash
sudo mkdir -p /var/lib/soj/seccomp
sudo install -m 0644 seccomp/soj-default.json /var/lib/soj/seccomp/
```

可以将 `DefaultSeccomp` 指向其他路径以使用更严格或更宽松的策略；参见[沙箱安全模型](#沙箱安全模型)部分了解完整的拒绝系统调用列表和 per-workflow 覆盖方式。

### 7. 预拉取评测基础镜像

演示题目使用 Debian。预先拉取以避免首次提交时的延迟：

```bash
apptainer pull /tmp/debian.sif docker://debian:latest
```

### 8. 编写 `config.yaml`

将 `config.yaml` 放在 `soj` 二进制文件同目录下。填入你的 SSH 公钥（用于登录的密钥）和第 1 步获取的评测用户 UID/GID：

```yaml
HostKey: |
  -----BEGIN OPENSSH PRIVATE KEY-----
  .../tmp/soj_host_key 文件内容...
  -----END OPENSSH PRIVATE KEY-----

ListenAddr: "0.0.0.0:2222"
APIAddr:    "0.0.0.0:8080"

# SSH 认证 — 选择一种模式（详见下方"SSH 认证"章节）：
#
# 模式一：单公钥（原版行为）
# AllowedSSHPubkey: "ssh-ed25519 AAAA... user@host"
#
# 模式二：GitHub 用户名列表 — 从 github.com/<user>.keys 拉取每个用户的公钥
# Auth:
#   Mode: github-list
#   GitHubUsers:
#     - "alice"
#     - "bob"
#   GitHubToken: "ghp_xxx"                   # 可选，避免 rate limit
#   GitHubEndpoint: "https://github.com"     # 可选，用于 GitHub Enterprise
#   KeyCachePath: "./keys_cache.json"        # 可选，密钥缓存文件路径（默认 ./keys_cache.json）

SubmitsDir:        /data/soj/submits
SubmitWorkDir:     /data/soj/work
RealSubmitsDir:    /data/soj/submits
RealSubmitWorkDir: /data/soj/work
ProblemsDir:       /data/soj/problems

SqlitePath: /data/soj/soj.db
SftpImage:  /tmp/soj-sftp.sif

SubmitUid: 942   # 来自 $JUDGE_UID
SubmitGid: 941   # 来自 $JUDGE_GID

Admins:
  - your-ssh-username

# 当 workflow 通过 mask: true 启用时屏蔽的敏感路径。
# 文件使用 /dev/null 覆盖；目录使用空目录覆盖。
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

# 安全封装默认值 — 应用于每个 workflow，除非单独覆盖。
# 完整配置表请参见"沙箱安全模型"。
DefaultNoPrivs: true                               # apptainer --no-privs
DefaultDropCaps: []                                # 额外剥离的权限（大写）
DefaultAddCaps: []                                 # 额外添加的权限（累加）
DefaultSeccomp: /var/lib/soj/seccomp/soj-default.json   # "" 则禁用
```

### 9. 编写演示题目

`/data/soj/problems/hello.yaml`:

```yaml
version: 1
id: hello
text: "提交包含数字 42 的 answer.txt 文件"
submits:
  - path: answer.txt
    isdir: false
workflow:
  - image: /tmp/debian.sif
    steps:
      - >-
        ans=$(cat /submits/answer.txt | tr -d '[:space:]');
        if [ "$ans" = "42" ]; then
          printf '{"success":true,"score":100,"message":"正确！答案为 %s"}' "$ans" > /result/result.json;
        else
          printf '{"success":true,"score":0,"message":"错误答案：得到 %s"}' "$ans" > /result/result.json;
        fi
    timeout: 15
    disablenetwork: true
    show: [1]
    mask: true
```

### 10. 启动 SOJ

`soj` 需要将每次提交的目录 `chown` 到评测用户 UID，因此以 root 运行：

```bash
sudo ./soj
```

### 11. 通过 SFTP 上传并提交

在另一个终端中，模拟真实用户操作：通过 SFTP 子系统使用 `scp` 上传文件，然后使用 `ssh ... submit` 触发评测。远程路径相对于 SFTP 容器内的 `/work`，对应宿主机上的 `SubmitsDir/<user>`，因此 `hello/answer.txt` 会落在 `/data/soj/submits/zambar/hello/answer.txt`。

```bash
echo "42" > /tmp/answer.txt

# 通过 SFTP 子系统容器上传
scp -P 2222 /tmp/answer.txt zambar@localhost:hello/answer.txt

# 运行评测
ssh -p 2222 zambar@localhost submit hello
```

预期输出末尾为：

```
得分 100.00 满分 100 (未加权)
评测消息:
    正确！答案为 42
```

尝试 `echo 99 > /tmp/answer.txt && scp -P 2222 /tmp/answer.txt zambar@localhost:hello/answer.txt` 后再次 `submit` 可查看错误答案路径。

> **旧版 OpenSSH 的 scp**
> OpenSSH 8.7 起 `scp` 开始使用 SFTP 协议（可选），9.0 起默认启用。
> `8.7 ≤ 版本 < 9.0` 需添加 `-s`；`< 8.7` 请直接使用 `sftp` 替代 `scp`。

---

## SSH 认证

SOJ 支持两种认证模式，通过 `config.yaml` 选择。两种模式下，客户端提供的 SSH 用户名即为 SOJ 用户身份。

### 模式一：单公钥 (`AllowedSSHPubkey`)

原版模式。仅接受一个公钥；`AllowedSSHPubkey` 留空则接受任意密钥（无用户名验证）。

```yaml
AllowedSSHPubkey: "ssh-ed25519 AAAA... user@host"
```

适用于单用户场景或测试。**无法防止用户名伪造** — 任何持有匹配密钥的客户端可以冒充任意用户名。

### 模式二：GitHub 用户名列表 (`Auth.Mode: github-list`)

SOJ 在启动时从 `https://github.com/<username>.keys` 拉取每个用户的公钥。SSH 用户名**必须与 GitHub 用户名一致** — 客户端提供的密钥在拉取的集合中查找，拥有该密钥的 GitHub 用户名必须等于 `ctx.User()`。

```yaml
Auth:
  Mode: github-list
  GitHubUsers:
    - "alice"
    - "bob"
    - "charlie"
  GitHubToken: "ghp_xxx"                   # 可选；避免 60 请求/小时的 rate limit
  GitHubEndpoint: "https://github.com"     # 可选；用于 GitHub Enterprise
```

**工作原理：**

1. 启动时，SOJ 检查缓存文件（默认 `./keys_cache.json`，可通过 `Auth.KeyCachePath` 配置）。如果缓存存在且未超过 24 小时，直接从磁盘加载密钥，不请求 GitHub。
2. 如果缓存不存在或已过期，SOJ 并发拉取所有列出用户的密钥（带进度条）。拉取失败会记录错误日志但**不会**阻止 SOJ 启动 — 失败的用户在密钥加载前无法登录。
3. 每个密钥通过 SHA256 指纹映射到其 GitHub 用户名。
4. SSH 连接时，查找客户端密钥的指纹。如果找到且拥有用户名与 `ctx.User()` 匹配，则认证成功。
5. 管理员可运行 `ssh oj adm refresh-keys` 重新拉取所有密钥，无需重启 SOJ（同时更新缓存）。

**要求：**

- SOJ 主机必须能通过 HTTPS 访问 `github.com`（或配置的端点）。
- 用户必须在其 GitHub 账户设置中配置至少一个 SSH 公钥（`Settings → SSH and GPG keys`）。
- 连接 SOJ 时使用的 SSH 用户名**必须与 GitHub 用户名完全一致**。

**向后兼容：** 如果未配置 `Auth` 但设置了 `AllowedSSHPubkey`，SOJ 自动回退到单公钥模式。如果两者都未设置，SOJ 接受任意密钥（开放模式）并输出警告。

---

## 配置参考

### 全局配置 (`config.yaml`)

| 字段 | 用途 |
|---|---|
| `HostKey` | SSH 主机私钥 (PEM) |
| `ListenAddr` / `APIAddr` | SSH 和 HTTP 监听地址 |
| `AllowedSSHPubkey` | 允许登录的 SSH 公钥；留空 = 接受任意密钥。旧版字段，推荐使用 `Auth` |
| `Auth.Mode` | 认证模式：`single`、`github-list` 或留空（自动推断） |
| `Auth.AllowedSSHPubkey` | `Auth.Mode=single` 时使用，等价于顶层 `AllowedSSHPubkey` |
| `Auth.GitHubUsers` | GitHub 用户名列表，启动时拉取其 SSH 公钥（`github-list` 模式） |
| `Auth.GitHubToken` | 可选的 GitHub 个人访问令牌，提升 rate limit（60→5000 请求/小时） |
| `Auth.GitHubEndpoint` | GitHub 基础 URL（默认 `https://github.com`）；用于 GitHub Enterprise |
| `Auth.KeyCachePath` | SSH 密钥缓存文件路径（默认 `./keys_cache.json`）。缓存 24 小时有效，避免重启时重复请求 GitHub |
| `SubmitsDir` | 上传提交存储位置 (`<dir>/<user>/<problem>/`) |
| `SubmitWorkDir` | 每次提交的临时工作目录（每次运行时创建和销毁） |
| `RealSubmitsDir` / `RealSubmitWorkDir` | 作为 `SOJ_REAL_*` 环境变量暴露给容器的宿主机路径。除非 SOJ 本身在容器中运行，否则与 *Dir 字段相同 |
| `ProblemsDir` | `*.yaml` 题目定义目录；文件名去掉 `.yaml` 即为题目 ID |
| `SqlitePath` | SQLite 数据库文件 |
| `SftpImage` | SFTP 子系统容器的 Apptainer 镜像（SIF 路径或 `docker://` URI） |
| `SubmitUid` / `SubmitGid` | 拥有提交文件并运行容器进程的 UNIX uid/gid |
| `Admins` | 拥有管理员命令权限的用户名 |
| `DefaultMaskFiles` | 当 workflow 设置 `mask: true` 且未覆盖 `maskfiles` 时，使用 `/dev/null` 屏蔽的路径 |
| `DefaultMaskDirs` | 同上条件下使用空目录屏蔽的路径 |
| `DefaultProperties` | 当 workflow 未设置 `properties:` 时应用于每一步的 `systemd-run --property=` 字符串（仅限 cgroup/超时类型的值） |
| `DefaultNoPrivs` | 为 true 时，每个 workflow 使用 `apptainer --no-privs`（剥离所有权限 + 设置 NoNewPrivs）。推荐 **true**。参见[安全模型](#沙箱安全模型)。 |
| `DefaultDropCaps` | 当 workflow 未设置 `dropcaps:` 时应用于每个 workflow 的额外 `--drop-caps`。与 `DefaultNoPrivs: false` 结合可剥离特定权限列表。 |
| `DefaultAddCaps` | 应用于每个 workflow 的 `--add-caps`。**累加**：workflow 的 `addcaps:` 是追加而非替换。除非每个 workflow 都确实需要同一权限，否则保持 `[]`。 |
| `DefaultSeccomp` | 应用于每个 workflow 的 OCI seccomp JSON 宿主机路径。`""` 则禁用。推荐：将 `seccomp/soj-default.json` 安装到 `/var/lib/soj/seccomp/`。 |

### 题目 YAML

```yaml
version: 1
id: <字符串>
text: <显示给用户的描述>
weight: 1.0                 # 分数倍率；默认 1.0
submits:
  - path: <SubmitsDir/<user>/<problem>/ 下的相对路径>
    isdir: false            # 设为 true 以遍历目录树
workflow:                   # 一个或多个阶段，顺序执行
  - image: <SIF 路径或 docker://URI>
    steps:                  # shell 命令；每步在新的 systemd scope 中运行
      - "..."
    timeout: 15             # 每步超时时间（秒）
    disablenetwork: true
    networkhostmode: false  # 当 disablenetwork 为 true 时忽略
    show: [1, 2]            # 输出流显示给用户的步骤编号（1 起始）
    mounts:                 # 在 /submits、/work、/result 之上的额外 bind 挂载
      - type: bind
        source: /host/path
        target: /container/path
        readonly: true
    properties: []          # systemd-run --property=… 覆盖；参见 DefaultProperties
    mask: true              # 参见下方屏蔽部分
    maskfiles: []           # 可选覆盖（空 = 使用 DefaultMaskFiles）
    maskdirs: []

    # --- 安全封装（完整配置表请参见"沙箱安全模型"） ---
    user: ""                # "" = SubmitUid; "root"/"0" = 容器 root; "<n>" = 数字 uid (n>0)
    privilegedsteps: []     # 跳过 setpriv uid 降低的步骤编号（1 起始）
    noprivs: true           # apptainer --no-privs（仅向上覆盖 DefaultNoPrivs）
    keepprivs: false        # apptainer --keep-privs；与 noprivs 互斥
    dropcaps: []            # nil ⇒ 回退到 DefaultDropCaps; 显式 [] ⇒ 不剥离额外权限
    addcaps: []             # 在 DefaultAddCaps 基础上累加
    seccomp: ""             # "" ⇒ DefaultSeccomp; 显式路径则覆盖
    noseccomp: false        # 设为 true 以完全禁用此 workflow 的默认配置
```

> **迁移说明**：旧的 `root: true` 字段已移除。替换为 `user: "root"`（或等效的 `user: "0"`）。yaml.v3 会静默忽略未知字段，因此未迁移的 workflow 会降级为以 `SubmitUid` 运行。

最后一个 workflow 的最后一步必须在 `/result/` 中留下 `result.json`：

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

`tag` 字段为可选字符串，会附带显示在分数旁（如 `90.00 (6.00x)`）。为空或省略则不显示。

每步可用的环境变量：

```
SOJ_SUBMITS_DIR=/submits        SOJ_REAL_SUBMITDIR=<宿主机路径>
SOJ_WORK_DIR=/work              SOJ_REAL_WORKDIR=<宿主机路径>
SOJ_RESULT_DIR=/result          SOJ_REAL_RESULTDIR=<宿主机路径>
SOJ_PROBLEM=<题目 ID>           SOJ_SUBMIT=<提交 ID>
SOJ_WORK_UID=<SubmitUid>        SOJ_WORK_GID=<SubmitGid>
```

### 路径屏蔽

Apptainer 的 `--no-mount proc,sys` 是全有或全无的，会破坏需要 `/proc/self/*` 的工具。SOJ 改用无害源 bind 覆盖特定敏感路径，保留 `/proc` 和 `/sys` 的其余部分可用。

per-workflow 配置与 `DefaultMaskFiles` / `DefaultMaskDirs` 的组合规则：

| Workflow 设置 | 实际屏蔽的文件/目录 |
|---|---|
| `mask: false` 或未设置 | 无 |
| `mask: true` | `DefaultMaskFiles` + `DefaultMaskDirs` |
| `mask: true` + `maskfiles: [...]` | 指定文件 + `DefaultMaskDirs` |
| `mask: true` + `maskdirs: [...]` | `DefaultMaskFiles` + 指定目录 |
| `mask: true` + 两者都设置 | 指定文件 + 指定目录（忽略默认值） |

### 评测器内置的沙箱默认值

以下在 `judge/evaluator.go` 中设置，应用于每个评测容器；目前不由 YAML 驱动：

| 行为 | 实现方式 |
|---|---|
| 只读根文件系统 | 不传入 `--writable-tmpfs`；SIF 层默认只读 |
| `--containall` | 始终传入 |
| `--net --network=none` | 当 `disablenetwork: true` 时 |
| 每步超时 | `systemd-run --scope --property=RuntimeMaxSec=<timeout>` |
| 用户名验证 | SFTP 会话仅允许 `[A-Za-z0-9_-]{1,64}` |
| 容器内 uid 降低 | `setpriv --reuid=<uid> --regid=<gid> --clear-groups --` 包裹每个非特权步骤 |
| 自动添加的权限 | 当 workflow 有任何 `uid > 0` 的非特权步骤时，自动注入 `CAP_SETUID` + `CAP_SETGID` 到 `--add-caps` — setpriv uid 降低所必需 |

---

## 沙箱安全模型

### 威胁模型

SOJ 提交是*不可信的用户代码*，默认在 Apptainer 容器中以 root 运行。没有以下防护层，该代码暴露的内核攻击面包括 io_uring、AF_ALG 加密套接字、`unshare(CLONE_NEWUSER)`、密钥环、`bpf(2)`、`perf_event_open(2)`、内核模块加载、kexec 以及 `mount` / `swap` / `quotactl` 系列 — 均为历史上可被利用的 LPE 原语。

SOJ 的防御是三层同心圆，加上一个主机级前置条件：

1. **内核级过滤 (seccomp)** — 在 `apptainer instance start` 时使用 `--security seccomp:<profile>`。默认配置为 `seccomp/soj-default.json`，拒绝上述系统调用。应用于容器产生的所有进程，包括通过 `apptainer exec` 产生的。
2. **权限边界 (apptainer `--no-privs` + `--add-caps`)** — 在实例启动时剥离所有 Linux 权限，然后仅添加 workflow 声明的权限以及 setpriv 所需的权限。
3. **容器内 uid 降低 (`setpriv`)** — 每个非特权步骤被 `setpriv --reuid=<uid> --regid=<gid> --clear-groups --` 包裹，使用户工作负载以 `SubmitUid` 运行而非容器 root，**其 effective/permitted/inheritable 集合中权限为零**（uid 0→非 0 转换默认清除这些权限）。
4. **主机级收口** — seccomp 无法检查指针参数，因此 `clone3(CLONE_NEWUSER)` **在我们的层面不可过滤**（详见 seccomp 配置部分）。主机必须设置 `kernel.unprivileged_userns_clone = 0` 来关闭此缺口；完整 sysctl + modprobe 列表参见[推荐的主机级加固](#推荐的主机级加固运维人员)。

### 执行模型 — 每步实际发生什么

```
宿主机 (root)
  └─> apptainer instance start
        --no-privs                    ← 剥离权限 + NoNewPrivs
        --add-caps CAP_SETUID,CAP_SETGID,<workflow.addcaps>
        --security seccomp:/path
        … bind 挂载 / 屏蔽 bind / 镜像 / 实例名

  └─> 对每一步:
        systemd-run --scope --property=RuntimeMaxSec=<timeout> ...
          apptainer exec --pwd /work instance://<name>
            [setpriv --reuid=<uid> --regid=<gid> --clear-groups --]   # 特权步骤跳过
              sh -c "<步骤命令>"
```

重要结论：**`addcaps` 在 setpriv uid 降低后不保留**。具有 `noprivs: true, addcaps: [CAP_SYS_NICE]` 和非特权步骤的 workflow 给予*步骤命令*零有效权限 — 该权限仅在 setpriv 运行前的短暂 root 窗口期存在。如果工作负载确实需要 CAP_SYS_NICE，可以 (a) 将步骤标记为 `privilegedsteps:` 跳过 setpriv（此时以容器 root 运行并保留该权限），或 (b) 依赖 cgroup 约束（`AllowedCPUs=`、`AllowedMemoryNodes=`）将 `sched_setaffinity` / `mbind` 的默认允许集绑定，而无需该权限。

### 默认 seccomp 配置 (`seccomp/soj-default.json`)

`defaultAction: SCMP_ACT_ALLOW`，对以下系统调用设置 `SCMP_ACT_ERRNO` 规则：

| 组 | 系统调用 |
|---|---|
| io_uring | `io_uring_setup` (425), `io_uring_enter` (426), `io_uring_register` (427) |
| 密钥环 | `add_key`, `request_key`, `keyctl` |
| 追踪 | `bpf`, `perf_event_open` |
| 模块 / kexec | `init_module`, `finit_module`, `delete_module`, `kexec_load`, `kexec_file_load` |
| mount 系列 | `pivot_root`, `mount`, `umount`, `umount2`, `move_mount`, `open_tree`, `fsopen`, `fsmount`, `fsconfig`, `fspick`, `swapon`, `swapoff`, `quotactl`, `quotactl_fd`, `nfsservctl` |
| 页缓存 LPE | `vmsplice` |
| 用户命名空间 | 当参数包含 `CLONE_NEWUSER` 位时的 `unshare` / `clone` / `setns` |
| 加密套接字 | `socket(AF_ALG, …)` (family=38) |
| 网络绕过 | `socket(AF_XDP, …)` (family=44) |

显式允许（不在任何拒绝规则中，由 default-allow 操作捕获）：`sched_setaffinity`、`sched_getaffinity`、`getcpu`、`set_mempolicy`、`get_mempolicy`、`mbind`、`setresuid`、`setresgid`、`setgroups` — 即 `numactl` 和 `setpriv` 所需的一切。

> **为什么 `clone3` 未被过滤。** 此配置的早期版本对 `clone3` 返回 ENOSYS 以便 glibc 回退到 `clone(2)`（我们对 `CLONE_NEWUSER` 位进行过滤）。glibc ≥ 2.37 移除了该回退并为 `pthread_create` 无条件调用 `clone3`，因此 `ENOSYS` 规则会在任何较新的主机上（Manjaro、Arch、Fedora 38+、Debian trixie）破坏 apptainer 启动器二进制文件。`clone3` 的标志参数位于指向的 `clone_args` 结构体中，seccomp 无法检查 — 因此唯一有用的选项是"允许"或"完全拒绝"；我们选择允许。要关闭由此产生的用户命名空间缺口，请应用下方列出的主机 sysctl。

该配置为 OCI runtime-spec 格式的 JSON；可以直接编辑 `/var/lib/soj/seccomp/soj-default.json` 或将 `DefaultSeccomp` 指向其他文件。

### 推荐的主机级加固（运维人员）

以下 sysctl 和模块黑名单补充了 per-container 封装，关闭了 seccomp 从容器内部无法触及的缺口。在 SOJ 主机上应用：

```bash
# /etc/sysctl.d/99-soj-hardening.conf
kernel.unprivileged_userns_clone = 0   # 阻止非特权用户命名空间创建
                                        # 即使通过 clone3(CLONE_NEWUSER) 也无法绕过
                                        # 我们无法在 seccomp 层面过滤它
kernel.io_uring_disabled         = 2   # 对 io_uring 系统调用过滤的双保险
                                        # （内核编译时可能启用了 CONFIG_IO_URING=y）
kernel.kptr_restrict             = 2
kernel.dmesg_restrict            = 1
kernel.unprivileged_bpf_disabled = 1
```

```bash
# /etc/modprobe.d/soj-hardening.conf — 阻止加密套接字攻击面
# 即使 CAP_SYS_MODULE 以某种方式可达
install af_alg          /bin/false
install algif_hash      /bin/false
install algif_skcipher  /bin/false
install algif_aead      /bin/false
install algif_rng       /bin/false
install xfrm_user       /bin/false
```

使用 `sudo sysctl --system` 和 `sudo update-initramfs -u`（或你的发行版等效命令）应用，然后重启。这些超出 SOJ 的控制范围 — 沙箱只能过滤内核愿意过滤的内容。

### Per-workflow 字段参考

以下每个字段独立覆盖（或增强）默认值。"与默认值的交互"列表示该字段中 `""` / `nil` / `false` 的含义。

| 字段 | 类型 | 含义 | 与默认值的交互 |
|---|---|---|---|
| `user` | string | 非特权步骤在容器内的有效 uid。`""` ⇒ `SubmitUid`；`"root"` 或 `"0"` ⇒ uid 0（全局跳过 setpriv）；`"<n>"` ⇒ 数字 uid（必须 `>0`）。 | 无全局默认值；始终回退到 `SubmitUid`。 |
| `privilegedsteps` | `[]int`（1 起始） | 此列表中的步骤跳过 setpriv 包裹，以容器 root 运行。用于需要访问 root 拥有路径的准备/清理工作。 | 无默认值。 |
| `noprivs` bool | 为实例启动添加 `--no-privs`（剥离所有权限 + NoNewPrivs）。 | 与 `DefaultNoPrivs` 取或 — workflow 只能添加，不能移除。要移除请使用 `keepprivs: true`。 |
| `keepprivs` | bool | 添加 `--keep-privs`，保留 root 的完整权限集。仅用于受信基础设施 workflow。 | 与 `noprivs` 互斥；如果两者都为 true，行为取决于 apptainer 版本 — 应避免。 |
| `dropcaps` | `[]string` | `apptainer --drop-caps CAP_FOO,CAP_BAR,…`。权限名不区分大小写，可带或不带 `CAP_` 前缀。 | `nil` ⇒ 回退到 `DefaultDropCaps`。显式 `[]` ⇒ 不剥离额外权限。 |
| `addcaps` | `[]string` | `apptainer --add-caps …`。权限仅在*特权*步骤中保留；非特权步骤在 setpriv uid 降低时丢失（见上文）。 | **在 `DefaultAddCaps` 基础上累加** — 非替换。 |
| `seccomp` | string | OCI seccomp JSON 的宿主机路径。 | `""` ⇒ 回退到 `DefaultSeccomp`，除非 `noseccomp: true`。 |
| `noseccomp` | bool | 完全禁用此 workflow 的默认 seccomp 配置（不影响显式 `seccomp:` 路径）。 | 默认 false。 |

### 推荐配方

**锁定的学生代码（默认）：** 依赖平台默认值 — 学生只需编写不带安全配置项的 `workflow:` 即可。

```yaml
workflow:
  - image: …
    timeout: …
    mask: true
    # 无其他配置 — 使用 DefaultNoPrivs + DefaultSeccomp + SubmitUid
```

**numactl / NUMA 绑定工作负载（proj3 风格）：** 全面锁定，让 cgroup 约束 numactl 而非赋予 CAP_SYS_NICE。

```yaml
workflow:
  - image: …
    noprivs: true                    # 措施一：剥离所有权限
    properties:                      # 措施二：cgroup 绑定亲和性集合
      - "AllowedCPUs=0-7"
      - "AllowedMemoryNodes=0"
      - "MemoryMax=4G"
      - "CPUQuota=800%"
    # 不使用 addcaps：CAP_SYS_NICE 在 setpriv 后不保留；cgroup 才是
    # 实际将绑定保持在允许的 CPU/节点集合内的机制。
```

**受信的 root 后处理（评测编写者）：** 阶段/构建以 `SubmitUid` 运行，然后受信的评分步骤以容器 root 运行。

```yaml
workflow:
  - image: …
    privilegedsteps: [3]   # 步骤 3（1 起始）跳过 setpriv
    steps:
      - "cp /submits/*.c /work/"                           # uid 942
      - "cd /work && make"                                 # uid 942
      - "/scaffold/judge.sh /work/output /result/result.json"  # uid 0
```

**Per-problem 权限添加（特权步骤需要）：**

```yaml
workflow:
  - image: …
    privilegedsteps: [4]
    addcaps: [CAP_SYS_PTRACE]   # 步骤 4（特权，无 setpriv）保留此权限
    # 非特权步骤在实例启动时仍会注入此权限，但在 setpriv uid 降低时丢失。
    # 实际上仅对步骤 4 有效。
```

**Per-problem seccomp 覆盖（例如需要 `unshare` 的题目）：**

```yaml
workflow:
  - image: …
    seccomp: /var/lib/soj/seccomp/permissive-unshare.json
    # 或者，完全禁用某个 workflow 的 seccomp：
    # noseccomp: true
```

### 为什么 `addcaps` 不会"直接生效"

Linux 权限模型在进程从 uid 0 转换到非零 uid 时清除其 permitted / effective / inheritable 集合（除非设置了 `SECBIT_KEEP_CAPS` 或 `--ambient-caps`）。SOJ 的 setpriv 调用两者都不设置，因此在 apptainer 实例启动时添加的权限仅在 setpriv 运行前的 *pre-setpriv* root shim 中可见。如果未来变更连接 `setpriv --ambient-caps`，AddCaps 将传递到工作负载 — 在此之前，请将 AddCaps 视为仅限特权步骤的机制。

---

## SSH 命令

SOJ 运行后，`ssh -p 2222 <user>@<host>` 打开交互式会话。常用命令：

| 命令 | 别名 | 用途 |
|---|---|---|
| `submit <problem_id>` | `sub` | 提交并运行评测 workflow |
| `list [page]` | `ls` | 列出你的提交 |
| `status <submit_id>` | `st` | 显示单个提交详情 |
| `describe [problem_id]` | `desc` | 无参数：列出所有题目 ID。有参数：显示 id、文本和所需提交 |
| `my` | | 你的每题最高分 |
| `rank` | `rk` | 排行榜；分数后附带 `(tag)` 后缀（如 `90.00 (6.00x)`） |
| `token` | | HTTP API 的 Token cookie |

SFTP 也作为子系统暴露（`sftp -P 2222 <user>@<host>`），用户进入 SFTP 容器内的 `/work`，对应宿主机上的 `SubmitsDir/<user>`。

管理员命令（前缀 `adm`）：

| 命令 | 用途 |
|---|---|
| `adm list [page]` | 列出所有用户的所有提交 |
| `adm status <submit_id>` | 查看任意提交详情 |
| `adm pause` | 全局暂停提交 |
| `adm refresh-keys` | 重新从 GitHub 拉取 SSH 密钥（仅 `github-list` 模式） |

关于**终端用户视角**（提交者如何与运行中的部署交互 — 上传约定、OpenSSH 版本差异、每个命令的示例），请参见 [`GUIDE_zh.md`](./GUIDE_zh.md)。本 README 是运维指南；`GUIDE_zh.md` 是给仅需提交题目用户的指南。

---

## 清理 / 卸载

停止 SOJ，然后移除运行时状态。评测用户和 `/data/soj` 在二进制重新编译后保留，仅在全新启动或完全移除部署时需要执行以下操作。

```bash
# 停止 SOJ 进程
sudo pkill -f './soj$'

# 停止所有残留的 apptainer 实例
apptainer instance list
apptainer instance stop --all   # 如果有以 root 启动的实例则添加 `sudo`

# 移除所有运行时数据（数据库、提交、工作目录、题目）
sudo rm -rf /data/soj

# 移除屏蔽路径源目录和已安装的 seccomp 配置
sudo rm -rf /var/lib/soj

# 移除预构建镜像和密钥
rm -f /tmp/soj-sftp.sif /tmp/soj-sftp /tmp/soj-sftp.def
rm -f /tmp/debian.sif
rm -f /tmp/soj_host_key /tmp/soj_host_key.pub
rm -f keys_cache.json

# 删除评测 UNIX 用户（及其 home 目录）
sudo userdel -r judge

# 可选：清除 Apptainer 镜像缓存
apptainer cache clean --force
```

如果 SOJ 以 root 运行，`~/.apptainer` 和 `/var/tmp` 下可能有 root 拥有的文件 — 使用 `sudo` 清理。

---

## 故障排除

| 症状 | 原因 |
|---|---|
| `apptainer instance start failed: unknown flag: --pwd` | 旧二进制文件；拉取最新代码后重新编译 |
| `Unknown assignment: LimitMEMLOCK=0` | systemd scope 单元拒绝仅限 service 的属性；拉取最新代码后重新编译 |
| `WARNING: ... readlink /proc/self/exe: no such file or directory` | workflow 使用 `mask: true` 且 `/proc` 被完全屏蔽。使用路径级屏蔽默认值替代 `--no-mount proc` |
| `failed to read result file` | 最后一个 workflow 步骤未写入 `/result/result.json` |
| `rejected sftp session: invalid username` | 用户名包含 `[A-Za-z0-9_-]` 之外的字符 — 需要以避免 `apptainer` 参数注入 |
| 提交卡在 `dead` | 上一次 SOJ 运行在评测过程中崩溃。启动时状态被重写为 `dead`；重新提交即可 |
| `exec: setpriv: not found` 或步骤立即退出 127 | 容器镜像缺少 `setpriv` (util-linux)。重新构建镜像并安装 `util-linux`，或将受影响步骤标记为 `privilegedsteps: [<n>]` 跳过 setpriv |
| `setpriv: setresuid: Operation not permitted` | 应用了 `noprivs: true` 但 bounding 集合中缺少 `CAP_SETUID/CAP_SETGID`。评测器会自动添加 — 如出现此错误，请检查 workflow 的 `dropcaps:` 是否再次剥离了这些权限，以及是否有主机级 seccomp 或 AppArmor 配置干扰 |
| `seccomp profile applied but workload still uses io_uring` | `apptainer --version` 输出不包含 seccomp — apptainer 未启用 libseccomp 编译，`--security seccomp:` 被静默忽略。重新编译 apptainer 以启用 seccomp 支持 |
| `apptainer instance start failed: invalid argument: --security seccomp:…` | `DefaultSeccomp` 指向缺失或格式错误的 JSON。使用 `python3 -m json.tool /var/lib/soj/seccomp/soj-default.json` 验证 |
| `runtime/cgo: pthread_create failed: Operation not permitted` 后段错误 | seccomp 配置过滤了 `clone3`（如返回 ENOSYS）。glibc ≥ 2.37 不再回退到 `clone(2)`，apptainer 启动器死亡。从配置中移除任何 `clone3` 规则；依赖主机级 `kernel.unprivileged_userns_clone = 0` |
| 旧 workflow 的 `root: true` 被静默忽略 | 该字段已移除；使用 `user: "root"`（或 `user: "0"`）。yaml.v3 不会对未知字段报错 |

---

## 致谢

本项目基于 [ZJUSCT/SOJ](https://github.com/ZJUSCT/SOJ)
