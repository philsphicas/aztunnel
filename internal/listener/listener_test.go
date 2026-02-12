package listener

import "testing"

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
