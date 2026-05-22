package scenarios

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

// sshServer is a minimal in-process SSH server used by
// ScenarioSSH_ProxyCommand. It accepts pubkey auth with a generated
// key pair and runs every exec request through /bin/sh -c locally.
type sshServer struct {
	addr    string
	keyPath string
	ln      net.Listener
}

// Addr returns the host:port the SSH server listens on.
func (s *sshServer) Addr() string { return s.addr }

// HostKeyPath returns the path to the private key, suitable as ssh
// -i argument; the same key is used as host key and client key
// because the scenario does StrictHostKeyChecking=no anyway.
func (s *sshServer) HostKeyPath() string { return s.keyPath }

// startSSHServer starts an in-process SSH server on a free localhost
// port. The listener is closed on t.Cleanup; any in-flight
// connections drop on close.
func startSSHServer(t *testing.T) *sshServer {
	t.Helper()

	dir := t.TempDir()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	keyPath := filepath.Join(dir, "test_key")
	pemBytes, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(pemBytes), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	authorizedKey := signer.PublicKey()

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
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
	t.Cleanup(func() { _ = ln.Close() })

	go sshAcceptLoop(ln, config)

	return &sshServer{
		addr:    ln.Addr().String(),
		keyPath: keyPath,
		ln:      ln,
	}
}

func sshAcceptLoop(ln net.Listener, config *ssh.ServerConfig) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleSSHConn(conn, config)
	}
}

func handleSSHConn(conn net.Conn, config *ssh.ServerConfig) {
	defer conn.Close() //nolint:errcheck // best-effort cleanup

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer sshConn.Close() //nolint:errcheck // best-effort cleanup
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, requests, err := newCh.Accept()
		if err != nil {
			return
		}
		go handleSSHSession(ch, requests)
	}
}

func handleSSHSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close() //nolint:errcheck // best-effort cleanup
	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		if len(req.Payload) < 4 {
			_ = req.Reply(false, nil)
			continue
		}
		cmdLen := int(req.Payload[0])<<24 | int(req.Payload[1])<<16 |
			int(req.Payload[2])<<8 | int(req.Payload[3])
		if len(req.Payload) < 4+cmdLen {
			_ = req.Reply(false, nil)
			continue
		}
		command := string(req.Payload[4 : 4+cmdLen])
		_ = req.Reply(true, nil)

		cmd := exec.Command("sh", "-c", command) //nolint:gosec // test fixture: ssh server runs only in scenarios
		cmd.Stdout = ch
		cmd.Stderr = ch.Stderr()
		cmd.Stdin = ch

		var exitStatus uint32
		if err := cmd.Run(); err != nil {
			exitStatus = 1
		}

		_, _ = ch.SendRequest("exit-status", false,
			ssh.Marshal(struct{ Status uint32 }{exitStatus}))
		return
	}
}
