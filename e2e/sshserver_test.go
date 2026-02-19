//go:build e2e

package e2e

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// sshServer is a minimal in-process SSH server for testing.
// It accepts pubkey auth with a generated key pair and handles
// exec requests by running commands locally.
type sshServer struct {
	addr    string
	keyPath string
	ln      net.Listener
}

// Addr returns the listen address.
func (s *sshServer) Addr() string { return s.addr }

// HostKeyPath returns the path to the private key (usable as -i for ssh).
func (s *sshServer) HostKeyPath() string { return s.keyPath }

// startSSHServer starts an in-process SSH server on a random port.
func startSSHServer(t *testing.T) *sshServer {
	t.Helper()

	dir := t.TempDir()

	// Generate an ed25519 key pair used as both host key and client key.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// Write private key to disk for the ssh client's -i flag.
	keyPath := filepath.Join(dir, "test_key")
	pemBytes, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(pemBytes), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	authorizedKey := signer.PublicKey()

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorizedKey.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("unknown key")
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { ln.Close() })

	go acceptLoop(t, ln, config)

	return &sshServer{
		addr:    ln.Addr().String(),
		keyPath: keyPath,
		ln:      ln,
	}
}

func acceptLoop(t *testing.T, ln net.Listener, config *ssh.ServerConfig) {
	t.Helper()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		go handleSSHConn(t, conn, config)
	}
}

func handleSSHConn(t *testing.T, conn net.Conn, config *ssh.ServerConfig) {
	t.Helper()
	defer conn.Close()

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, requests, err := newCh.Accept()
		if err != nil {
			return
		}
		go handleSession(ch, requests)
	}
}

func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				req.Reply(false, nil)
			}
			continue
		}
		// Payload is: uint32 length + command string.
		if len(req.Payload) < 4 {
			req.Reply(false, nil)
			continue
		}
		cmdLen := int(req.Payload[0])<<24 | int(req.Payload[1])<<16 | int(req.Payload[2])<<8 | int(req.Payload[3])
		if len(req.Payload) < 4+cmdLen {
			req.Reply(false, nil)
			continue
		}
		command := string(req.Payload[4 : 4+cmdLen])
		req.Reply(true, nil)

		cmd := exec.Command("sh", "-c", command)
		cmd.Stdout = ch
		cmd.Stderr = ch.Stderr()
		cmd.Stdin = ch

		var exitStatus uint32
		if err := cmd.Run(); err != nil {
			exitStatus = 1
		}

		// Send exit-status.
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exitStatus}))
		return
	}
}
