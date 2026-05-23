package file_transfer

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	ssh "github.com/gliderlabs/ssh"
	"github.com/mrhaoxx/SOJ/types"
)

// appendIfMissing 大小写不敏感地把 want 中不在 caps 里的项追加进去。
func appendIfMissing(caps []string, want ...string) []string {
	have := make(map[string]struct{}, len(caps))
	for _, c := range caps {
		have[strings.ToUpper(c)] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[strings.ToUpper(w)]; ok {
			continue
		}
		caps = append(caps, w)
		have[strings.ToUpper(w)] = struct{}{}
	}
	return caps
}

// isValidUsername rejects usernames that could inject arguments into apptainer commands.
func isValidUsername(u string) bool {
	if len(u) == 0 || len(u) > 64 {
		return false
	}
	for _, c := range u {
		if !('a' <= c && c <= 'z') && !('A' <= c && c <= 'Z') &&
			!('0' <= c && c <= '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// SftpHandler handler for SFTP subsystem
func SftpHandler(sess ssh.Session, cfg *types.Config, sandboxService *ApptainerService) {
	if !isValidUsername(sess.User()) {
		log.Println("rejected sftp session: invalid username", sess.User())
		return
	}
	name := "soj-subsystem-sftp-" + sess.User() + "-" + time.Now().Format("20060102150405")
	path := cfg.SubmitsDir + "/" + sess.User()
	log.Println("new sftp session", sess.User(), name, path)

	if err := os.MkdirAll(path, 0700); err != nil {
		log.Println(name, "failed to create working dir", path, err)
		return
	}

	os.Chown(path, cfg.SubmitUid, cfg.SubmitGid)

	// 容器内 setpriv 在 busybox 镜像里不带 --reuid（applet 是 util-linux 的子集），
	// 所以 SFTP 走自降权路径：实例以容器 root 启动并保留 CAP_SETUID/CAP_SETGID，
	// /soj-sftp 启动后通过 SOJ_DROP_UID/GID 环境变量在 Go 里自己 setresuid 到
	// SubmitUid，这样上传的文件在宿主上就是 SubmitUid 所有，且不依赖镜像里有没有
	// 完整版 setpriv。
	addCaps := append([]string{}, cfg.DefaultAddCaps...)
	if cfg.SubmitUid > 0 {
		addCaps = appendIfMissing(addCaps, "CAP_SETUID", "CAP_SETGID")
	}

	success, id := sandboxService.RunImage(RunImageOpts{
		Name:     name,
		Hostname: "soj-sftpd",
		Image:    cfg.SftpImage,
		Workdir:  "/",
		Mounts: []types.Mount{
			{
				Type:   "bind",
				Source: path,
				Target: "/work",
			},
		},
		MaskFiles:      cfg.DefaultMaskFiles,
		MaskDirs:       cfg.DefaultMaskDirs,
		ReadonlyRootfs: true,
		Timeout:        120,
		Env: []string{
			"SOJ_DROP_UID=" + strconv.Itoa(cfg.SubmitUid),
			"SOJ_DROP_GID=" + strconv.Itoa(cfg.SubmitGid),
		},
		NoPrivs:  cfg.DefaultNoPrivs,
		DropCaps: cfg.DefaultDropCaps,
		AddCaps:  addCaps,
		Seccomp:  cfg.DefaultSeccomp,
	})

	if !success {
		log.Println(name, "failed to run sftp container")
		return
	}
	defer sandboxService.CleanContainer(id)

	log.Println(name, "running sftp stdio proxy to container", id)

	// /soj-sftp 自己处理 uid 降权，所以这里走 Privileged: true 跳过外层 setpriv 包装。
	_, _, err := sandboxService.ExecContainer(id, ExecOpts{
		Cmd:        "/soj-sftp stdio",
		Timeout:    3600,
		Stdin:      sess,
		Stdout:     sess,
		Stderr:     os.Stderr,
		Privileged: true,
	})
	if err != nil {
		log.Println(name, "failed to run stdio server in container", id, err)
		return
	}
	log.Println(name, "sftp session completed", id)

	log.Println(name, "session closed", id)
}
