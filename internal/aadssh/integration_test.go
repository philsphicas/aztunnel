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

// TestCertRoundTripWithRealKeygen exercises writeCert against a certificate
// produced by a real ssh-keygen, validating that our file formatting and Go
// certificate parsing agree with the tool. It skips when ssh-keygen is
// unavailable (e.g. minimal CI images).
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
		cmd := exec.Command(keygen, args...) //nolint:gosec // test fixtures use a trusted ssh-keygen path
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

	// Parse the round-tripped certificate with the same Go code the reuse path
	// uses, confirming our file format matches what ssh-keygen produced.
	cert, err := readCertificate(roundTripped)
	if err != nil {
		t.Fatalf("readCertificate: %v", err)
	}

	user, err := certificateUsername(cert)
	if err != nil {
		t.Fatalf("certificateUsername: %v", err)
	}
	if user != "alice@contoso.com" {
		t.Errorf("username = %q, want alice@contoso.com (lowercased first principal)", user)
	}
	if len(cert.ValidPrincipals) != 2 {
		t.Errorf("principals = %v, want 2 entries", cert.ValidPrincipals)
	}
	if !certStillValid(cert, time.Now(), time.Minute) {
		t.Errorf("cert valid for ~1h should still be valid, ValidBefore=%d", cert.ValidBefore)
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
		cmd := exec.Command(keygen, args...) //nolint:gosec // test fixtures use a trusted ssh-keygen path
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

	minValid := time.Minute
	opts := Options{
		Identity:    id,
		CertPath:    id + "-cert.pub",
		MinValidity: &minValid,
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
