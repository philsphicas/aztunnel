package aadssh

import (
	"crypto/rand"
	"crypto/rsa"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCertRoundTripWithRealKeygen exercises writeCert + inspectCert against a
// certificate produced by a real ssh-keygen, validating that our file
// formatting and `ssh-keygen -L` parsing agree with the tool. It skips when
// ssh-keygen is unavailable (e.g. minimal CI images).
func TestCertRoundTripWithRealKeygen(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not found in PATH")
	}

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca")
	id := filepath.Join(dir, "id")

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(keygen, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	// Create a CA key and a user key (no passphrase, quiet).
	run("-q", "-t", "rsa", "-b", "2048", "-N", "", "-f", ca)
	run("-q", "-t", "rsa", "-b", "2048", "-N", "", "-f", id)

	// Sign the user key with two principals, valid for one hour.
	run("-s", ca, "-I", "testid", "-n", "Alice@Contoso.com,alice", "-V", "+1h", id+".pub")

	signedCert := id + "-cert.pub"
	body, err := os.ReadFile(signedCert)
	if err != nil {
		t.Fatalf("read signed cert: %v", err)
	}

	// The signed cert file is "ssh-rsa-cert-v01@openssh.com <body> comment".
	// Extract just the base64 body (as Entra ID would return it) and round-trip
	// it through writeCert.
	parts := strings.Fields(string(body))
	if len(parts) < 2 {
		t.Fatalf("unexpected cert file contents: %q", string(body))
	}
	certBody := parts[1]

	roundTripped := filepath.Join(dir, "rt-cert.pub")
	if err := writeCert(roundTripped, certBody); err != nil {
		t.Fatalf("writeCert: %v", err)
	}

	info, err := inspectCert(keygen, roundTripped)
	if err != nil {
		t.Fatalf("inspectCert: %v", err)
	}

	user, err := info.username()
	if err != nil {
		t.Fatalf("username: %v", err)
	}
	if user != "alice@contoso.com" {
		t.Errorf("username = %q, want alice@contoso.com (lowercased first principal)", user)
	}
	if len(info.principals) != 2 {
		t.Errorf("principals = %v, want 2 entries", info.principals)
	}
	if !info.stillValid(time.Now(), time.Minute) {
		t.Errorf("cert valid for ~1h should still be valid, validTo=%v", info.validTo)
	}
}

func TestReuseExistingRejectsMismatchedPrivateKey(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not found in PATH")
	}

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca")
	id := filepath.Join(dir, "id")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(keygen, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("-q", "-t", "rsa", "-b", "2048", "-N", "", "-f", ca)

	original, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateKey(id, original); err != nil {
		t.Fatal(err)
	}
	if err := writePublicKey(id+".pub", original); err != nil {
		t.Fatal(err)
	}
	run("-s", ca, "-I", "testid", "-n", "alice@contoso.com", "-V", "+1h", id+".pub")

	opts := Options{
		Identity:    id,
		CertPath:    id + "-cert.pub",
		SSHKeygen:   keygen,
		MinValidity: time.Minute,
	}
	if _, ok := reuseExisting(&opts); !ok {
		t.Fatal("matching private key and certificate should be reusable")
	}

	replacement, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateKey(id, replacement); err != nil {
		t.Fatal(err)
	}
	if _, ok := reuseExisting(&opts); ok {
		t.Fatal("certificate must not be reused with a different private key")
	}
}
