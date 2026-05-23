package main

import (
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"strconv"
	"syscall"

	"github.com/mrhaoxx/sftp"
)

type structRW struct {
	io.Reader
	io.Writer
}

func (s *structRW) Close() error {
	return nil
}

// dropPrivilege drops uid/gid to SOJ_DROP_UID / SOJ_DROP_GID if both env
// vars are set to positive integers. SOJ launches /soj-sftp as container
// root (so CAP_SETUID/CAP_SETGID are available); this drops to SubmitUid
// before serving any SFTP request, so uploaded files end up SubmitUid-owned
// on the host and a compromised sftp-server can't write as root.
// Doing the drop in-process avoids depending on a particular flavour of
// setpriv (e.g. busybox's applet doesn't support --reuid).
func dropPrivilege() {
	uidStr := os.Getenv("SOJ_DROP_UID")
	gidStr := os.Getenv("SOJ_DROP_GID")
	if uidStr == "" || gidStr == "" {
		return
	}
	uid, err := strconv.Atoi(uidStr)
	if err != nil || uid <= 0 {
		log.Fatalf("SOJ_DROP_UID must be a positive integer, got %q", uidStr)
	}
	gid, err := strconv.Atoi(gidStr)
	if err != nil || gid <= 0 {
		log.Fatalf("SOJ_DROP_GID must be a positive integer, got %q", gidStr)
	}
	// Order matters: clear supplementary groups and drop primary gid while
	// still privileged, then drop uid (which wipes remaining caps).
	if err := syscall.Setgroups(nil); err != nil {
		log.Fatalf("setgroups: %v", err)
	}
	if err := syscall.Setresgid(gid, gid, gid); err != nil {
		log.Fatalf("setresgid(%d): %v", gid, err)
	}
	if err := syscall.Setresuid(uid, uid, uid); err != nil {
		log.Fatalf("setresuid(%d): %v", uid, err)
	}
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			log.Println("recovered from panic", r)
		}
	}()

	dropPrivilege()

	if len(os.Args) > 1 && os.Args[1] == "stdio" {
		serverOptions := []sftp.ServerOption{
			sftp.WithDebug(os.Stderr),
			sftp.WithServerWorkingDirectory("/work"),
		}
		
		rw := &structRW{os.Stdin, os.Stdout}
		server, err := sftp.NewServer(rw, serverOptions...)
		if err != nil {
			log.Fatalf("sftp server init error: %s\n", err)
		}
		if err := server.Serve(); err == io.EOF {
			server.Close()
			os.Exit(0)
		} else if err != nil {
			log.Fatalf("sftp server completed with error: %v", err)
		}
		os.Exit(0)
	}

	lc, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(netip.MustParseAddrPort("0.0.0.0:2207")))
	if err != nil {
		log.Fatalf("failed to listen on 0.0.0.0:2207")
	}

	log.Println("sftp server listening")

	conn, err := lc.AcceptTCP()
	if err != nil {
		log.Fatalf("failed to accept connection")
	}

	serverOptions := []sftp.ServerOption{
		//sftp.WithDebug(os.Stdout),
		sftp.WithServerWorkingDirectory("/work"),
	}
	server, err := sftp.NewServer(conn, serverOptions...)
	if err != nil {
		log.Printf("sftp server init error: %s\n", err)
		return
	}
	log.Println("sftp server working")

	if err := server.Serve(); err == io.EOF {
		server.Close()
		log.Println("sftp client exited session.")
	} else if err != nil {
		log.Println("sftp server completed with error:", err)
	}

	log.Println("sftp server exited")
}
