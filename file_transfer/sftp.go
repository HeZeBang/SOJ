package file_transfer

import (
	"log"
	"os"
	"strconv"
	"time"

	ssh "github.com/gliderlabs/ssh"
	"github.com/mrhaoxx/SOJ/types"
)

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

	success, id := sandboxService.RunImage(name, strconv.Itoa(cfg.SubmitUid), "soj-sftpd", cfg.SftpImage, "/", []types.Mount{
		{
			Type:   "bind",
			Source: path,
			Target: "/work",
		},
	}, cfg.DefaultMaskFiles, cfg.DefaultMaskDirs, true, false, 120, false, nil)

	if !success {
		log.Println(name, "failed to run sftp container")
		return
	}
	defer sandboxService.CleanContainer(id)

	log.Println(name, "running sftp stdio proxy to container", id)

	_, _, err := sandboxService.ExecContainer(id, "/soj-sftp stdio", 3600, sess, sess, os.Stderr, nil, false, nil)
	if err != nil {
		log.Println(name, "failed to run stdio server in container", id, err)
		return
	}
	log.Println(name, "sftp session completed", id)

	log.Println(name, "session closed", id)
}
