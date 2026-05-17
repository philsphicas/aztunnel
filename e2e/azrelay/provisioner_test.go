package azrelay

import (
	"strings"
	"testing"
)

func TestHycoNamePattern(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"valid entra", "e2e-entra-0123456789ab", true},
		{"valid sas", "e2e-sas-abcdef012345", true},
		{"static entra rejected", "e2e-entra", false},
		{"static sas rejected", "e2e-sas", false},
		{"short suffix", "e2e-entra-0123456789a", false},
		{"long suffix", "e2e-entra-0123456789abc", false},
		{"uppercase suffix", "e2e-entra-0123456789AB", false},
		{"non-hex suffix", "e2e-entra-0123456789xy", false},
		{"wrong middle", "e2e-foo-0123456789ab", false},
		{"unrelated name", "production-hyco", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HycoNamePattern.MatchString(tc.in)
			if got != tc.want {
				t.Errorf("HycoNamePattern.MatchString(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewSuffix(t *testing.T) {
	const want = suffixLen
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		s, err := newSuffix()
		if err != nil {
			t.Fatalf("newSuffix: %v", err)
		}
		if len(s) != want {
			t.Fatalf("newSuffix length = %d, want %d", len(s), want)
		}
		if strings.ToLower(s) != s {
			t.Fatalf("newSuffix not lowercase: %q", s)
		}
		for _, c := range s {
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !isHex {
				t.Fatalf("newSuffix has non-hex char %q in %q", c, s)
			}
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("newSuffix collision after 64 iterations: %q", s)
		}
		seen[s] = struct{}{}
		// Verify the generated suffix produces hyco names that the
		// janitor's regex will accept.
		if !HycoNamePattern.MatchString("e2e-entra-" + s) {
			t.Fatalf("generated suffix %q does not satisfy HycoNamePattern", s)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "missing subscription",
			cfg:     Config{ResourceGroup: "rg", Namespace: "ns"},
			wantErr: "SubscriptionID",
		},
		{
			name:    "missing resource group",
			cfg:     Config{SubscriptionID: "sub", Namespace: "ns"},
			wantErr: "ResourceGroup",
		},
		{
			name:    "missing namespace",
			cfg:     Config{SubscriptionID: "sub", ResourceGroup: "rg"},
			wantErr: "Namespace",
		},
		{
			name: "all set",
			cfg:  Config{SubscriptionID: "sub", ResourceGroup: "rg", Namespace: "ns"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate: expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate: error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestResultEnvVars(t *testing.T) {
	r := &Result{
		RelayName:       "my-relay",
		EntraHycoName:   "e2e-entra-0123456789ab",
		SASHycoName:     "e2e-sas-0123456789ab",
		ListenerKeyName: "listener",
		ListenerKey:     "lk",
		SenderKeyName:   "sender",
		SenderKey:       "sk",
	}
	want := map[string]string{
		"E2E_RELAY_NAME":            "my-relay",
		"E2E_ENTRA_HYCO_NAME":       "e2e-entra-0123456789ab",
		"E2E_SAS_HYCO_NAME":         "e2e-sas-0123456789ab",
		"E2E_SAS_LISTENER_KEY_NAME": "listener",
		"E2E_SAS_LISTENER_KEY":      "lk",
		"E2E_SAS_SENDER_KEY_NAME":   "sender",
		"E2E_SAS_SENDER_KEY":        "sk",
	}
	got := r.EnvVars()
	if len(got) != len(want) {
		t.Fatalf("EnvVars: len = %d, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("EnvVars[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestProvisionerSingleUse(t *testing.T) {
	// A Provisioner is single-use: a second Provision call must fail
	// without contacting Azure. Construct one with a sentinel Result
	// already populated and confirm Provision returns the expected error.
	p := &Provisioner{result: &Result{}}
	if _, err := p.Provision(t.Context()); err == nil {
		t.Fatal("expected error on reuse")
	} else if !strings.Contains(err.Error(), "already used") {
		t.Fatalf("error %q does not mention reuse", err.Error())
	}
}

func TestProvisionerHycoNamesUsesSuffix(t *testing.T) {
	p := &Provisioner{suffix: "0123456789ab"}
	entra, sas := p.HycoNames()
	if entra != "e2e-entra-0123456789ab" {
		t.Errorf("entra name = %q", entra)
	}
	if sas != "e2e-sas-0123456789ab" {
		t.Errorf("sas name = %q", sas)
	}
	for _, n := range []string{entra, sas} {
		if !HycoNamePattern.MatchString(n) {
			t.Errorf("generated name %q does not satisfy HycoNamePattern", n)
		}
	}
}
