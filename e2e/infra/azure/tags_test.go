package azure

import (
	"errors"
	"testing"
)

func TestAssertOwnedForDelete(t *testing.T) {
	ours := TagValTool
	other := "manual"
	callerOID := "11111111-1111-1111-1111-111111111111"
	otherOID := "00000000-0000-0000-0000-000000000000"
	cases := []struct {
		name string
		tags map[string]*string
		want error
	}{
		{"nil tags", nil, ErrNotOwned},
		{"no tool tag", map[string]*string{"unrelated": ptr("x")}, ErrNotOwned},
		{"other tool", map[string]*string{TagKeyTool: &other}, ErrNotOwned},
		{"nil value", map[string]*string{TagKeyTool: nil}, ErrNotOwned},
		{"missing owner", map[string]*string{TagKeyTool: &ours}, ErrNotOwned},
		{"owner mismatch", map[string]*string{TagKeyTool: &ours, TagKeyOwner: &otherOID}, ErrNotOwned},
		{"e2e-infra owned by caller", map[string]*string{TagKeyTool: &ours, TagKeyOwner: &callerOID}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AssertOwnedForDelete(c.tags, callerOID)
			if !errors.Is(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestOwnerTags(t *testing.T) {
	tags := ownerTags("11111111-1111-1111-1111-111111111111")
	if tags[TagKeyTool] == nil || *tags[TagKeyTool] != TagValTool {
		t.Errorf("tool tag missing or wrong: %v", tags[TagKeyTool])
	}
	if tags[TagKeyOwner] == nil || *tags[TagKeyOwner] != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("owner tag missing or wrong: %v", tags[TagKeyOwner])
	}
}

func ptr(s string) *string { return &s }
