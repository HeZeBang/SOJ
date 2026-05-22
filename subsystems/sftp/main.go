package main

import (
	"io"
	"log"
	"net"
	"net/netip"
	"os"

	"github.com/mrhaoxx/sftp"
)

type structRW struct {
	io.Reader
	io.Writer
}

func (s *structRW) Close() error {
	return nil
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			log.Println("recovered from panic", r)
		}
	}()

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
