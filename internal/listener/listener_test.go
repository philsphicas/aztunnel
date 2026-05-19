package listener

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"

	"github.com/philsphicas/aztunnel/internal/protocol"
)

func TestIsAllowed(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		allowList []string
		want      bool
	}{
		{"wildcard", "10.0.0.1:22", []string{"*"}, true},
		{"exact match", "10.0.0.1:22", []string{"10.0.0.1:22"}, true},
		{"exact no match", "10.0.0.1:22", []string{"10.0.0.2:22"}, false},
		{"wrong port", "10.0.0.1:80", []string{"10.0.0.1:22"}, false},
		{"cidr match", "10.0.0.5:22", []string{"10.0.0.0/8:22"}, true},
		{"cidr wildcard port", "10.0.0.5:8080", []string{"10.0.0.0/8:*"}, true},
		{"cidr no match", "192.168.0.1:22", []string{"10.0.0.0/8:22"}, false},
		{"multiple entries", "10.0.0.5:22", []string{"192.168.0.0/16:*", "10.0.0.0/8:22"}, true},
		{"hostname exact", "myhost:22", []string{"myhost:22"}, true},
		{"hostname wrong", "myhost:22", []string{"other:22"}, false},
		{"empty target", "", []string{"*"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAllowed(tt.target, tt.allowList)
			if got != tt.want {
				t.Errorf("isAllowed(%q, %v) = %v, want %v", tt.target, tt.allowList, got, tt.want)
			}
		})
	}
}

func TestSplitAllowEntry(t *testing.T) {
	tests := []struct {
		entry    string
		wantHost string
		wantPort string
		wantErr  bool
	}{
		{"10.0.0.1:22", "10.0.0.1", "22", false},
		{"10.0.0.0/8:*", "10.0.0.0/8", "*", false},
		{"myhost:22", "myhost", "22", false},
		{"nocolon", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			h, p, err := splitAllowEntry(tt.entry)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if h != tt.wantHost {
				t.Errorf("host = %q, want %q", h, tt.wantHost)
			}
			if p != tt.wantPort {
				t.Errorf("port = %q, want %q", p, tt.wantPort)
			}
		})
	}
}

func TestClassifyDialError_Nil(t *testing.T) {
	if got := classifyDialError(nil); got != "" {
		t.Errorf("classifyDialError(nil) = %q, want %q", got, "")
	}
}

func TestClassifyDialError_DNSNotFound(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "nonexistent.invalid", IsNotFound: true}
	if got := classifyDialError(err); got != protocol.CodeDNSNotFound {
		t.Errorf("classifyDialError(DNSError{IsNotFound}) = %q, want %q", got, protocol.CodeDNSNotFound)
	}
}

func TestClassifyDialError_DNSTimeout(t *testing.T) {
	err := &net.DNSError{Err: "i/o timeout", Name: "slow.example", IsTimeout: true}
	if got := classifyDialError(err); got != protocol.CodeDNSTimeout {
		t.Errorf("classifyDialError(DNSError{IsTimeout}) = %q, want %q", got, protocol.CodeDNSTimeout)
	}
}

func TestClassifyDialError_DNSTemporary(t *testing.T) {
	// Non-timeout, non-not-found DNS errors (e.g. SERVFAIL) fall through
	// to CodeDNSNotFound under the current spec. This documents that
	// behaviour so a future refinement (e.g. a separate dns_failed code)
	// is a deliberate change rather than an accidental one.
	err := &net.DNSError{Err: "server misbehaving", Name: "example.invalid"}
	if got := classifyDialError(err); got != protocol.CodeDNSNotFound {
		t.Errorf("classifyDialError(DNSError{plain}) = %q, want %q", got, protocol.CodeDNSNotFound)
	}
}

func TestClassifyDialError_DNSWrappedInOpError(t *testing.T) {
	// net.Dialer wraps DNS failures inside *net.OpError; classifyDialError
	// must unwrap via errors.As to find the underlying *net.DNSError.
	dnsErr := &net.DNSError{Err: "no such host", Name: "nonexistent.invalid", IsNotFound: true}
	wrapped := &net.OpError{Op: "dial", Net: "tcp", Err: dnsErr}
	if got := classifyDialError(wrapped); got != protocol.CodeDNSNotFound {
		t.Errorf("classifyDialError(OpError wrapping DNSError) = %q, want %q", got, protocol.CodeDNSNotFound)
	}
}

func TestClassifyDialError_ContextDeadlineBeatsDNSTimeout(t *testing.T) {
	// When the dial error is both a DNS timeout AND ctx.DeadlineExceeded,
	// the context-deadline branch must win: the operator deliberately
	// cancelled, so CodeTimeout reflects that intent better than the
	// underlying DNS-layer detail.
	dnsErr := &net.DNSError{Err: "i/o timeout", Name: "slow.example", IsTimeout: true}
	combined := errors.Join(context.DeadlineExceeded, dnsErr)
	if got := classifyDialError(combined); got != protocol.CodeTimeout {
		t.Errorf("classifyDialError(Join(DeadlineExceeded, DNS timeout)) = %q, want %q",
			got, protocol.CodeTimeout)
	}
}

func TestClassifyDialError_OtherErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, protocol.CodeConnectionRefused},
		{"host unreachable", &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}, protocol.CodeHostUnreachable},
		{"net unreachable", &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}, protocol.CodeNetworkUnreachable},
		{"context deadline", context.DeadlineExceeded, protocol.CodeTimeout},
		{"unclassified", errors.New("something broke"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyDialError(tt.err); got != tt.want {
				t.Errorf("classifyDialError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
