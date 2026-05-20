package azure

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/philsphicas/aztunnel/e2e/infra/internal/msgraph"
)

// DefaultResourceGroupPrefix is the prefix prepended to the slugged
// alias to form the per-developer default RG name. CI uses the bare
// CIResourceGroup ("aztunnel-e2e"); developers default to
// "aztunnel-e2e-<alias>" so they never collide with CI or each other.
const DefaultResourceGroupPrefix = "aztunnel-e2e-"

// CIResourceGroup is the fixed RG name that CI provisions and the
// `make e2e-ci` target falls back to when no override is provided.
const CIResourceGroup = "aztunnel-e2e"

// maxAliasLen caps the slugged alias length. Azure RGs allow 90 chars
// but the relay namespace name is derived from sub + RG and lives in
// a 50-char DNS label, so we leave headroom.
const maxAliasLen = 30

// SlugAlias returns a conservative slug suitable for embedding in
// an Azure RG name. The output:
//   - is lowercase ASCII letters, digits, or '-';
//   - has no consecutive '-';
//   - has no leading or trailing '-' or '.';
//   - is at most maxAliasLen characters;
//   - is empty when no slug can be produced.
//
// Unicode letters / digits are folded to their lowercase ASCII
// equivalents by a small map of common cases (accented Latin
// letters); anything else outside [a-z0-9-] becomes a '-' separator.
// The fold is intentionally minimal — when in doubt we let the
// caller fall back to the object-id-prefix path.
func SlugAlias(in string) string {
	in = strings.ToLower(in)
	var b strings.Builder
	b.Grow(len(in))
	prevDash := true
	for _, r := range in {
		if folded, ok := asciiFold[r]; ok {
			r = folded
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-.")
	if len(out) > maxAliasLen {
		out = strings.TrimRight(out[:maxAliasLen], "-.")
	}
	return out
}

// asciiFold maps a small set of accented Latin runes to their ASCII
// equivalent. Anything not in this map is treated as a separator.
var asciiFold = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u',
	'ñ': 'n', 'ç': 'c', 'ß': 's',
}

// AliasFromUPN returns a slug derived from the local-part of an UPN
// (the part before '@'). Returns "" when slugging yields nothing
// usable; callers should fall back to AliasFromObjectID in that case.
func AliasFromUPN(upn string) string {
	local := upn
	if at := strings.IndexByte(local, '@'); at >= 0 {
		local = local[:at]
	}
	return SlugAlias(local)
}

// AliasFromObjectID returns the first 12 hex characters of an AAD
// object id, stripping hyphens. The 48-bit prefix is enough to make
// collisions across developers in one subscription statistically
// negligible while keeping the RG name short.
func AliasFromObjectID(oid string) string {
	clean := strings.ToLower(strings.ReplaceAll(oid, "-", ""))
	if len(clean) > 12 {
		clean = clean[:12]
	}
	return clean
}

// DeriveAlias picks the alias from the signed-in user. UPN local-part
// is preferred; falls back to the first 12 hex of the object id if
// slugging produces nothing usable.
func DeriveAlias(oid, upn string) string {
	if a := AliasFromUPN(upn); a != "" {
		return a
	}
	return AliasFromObjectID(oid)
}

// ResolveResourceGroup applies the documented precedence for picking
// the e2e RG name. Called before Provisioner construction because
// NewProvisioner requires the resolved RG up-front.
//
//  1. Explicit `--resource-group` / RESOURCE_GROUP wins.
//  2. Explicit `--alias` / ALIAS → DefaultResourceGroupPrefix + slug.
//  3. ciFallback (CICmd path) → CIResourceGroup.
//  4. Otherwise → derive from signed-in user (msgraph /me).
//
// The msgraph lookup happens only on the fallback path so commands
// invoked with an explicit RG name never pay for it.
func ResolveResourceGroup(ctx context.Context, cred azcore.TokenCredential, explicitRG, explicitAlias string, ciFallback bool) (string, error) {
	if explicitRG != "" {
		return explicitRG, nil
	}
	if explicitAlias != "" {
		slug := SlugAlias(explicitAlias)
		if slug == "" {
			return "", fmt.Errorf("--alias %q produces an empty slug; pass --resource-group explicitly", explicitAlias)
		}
		return DefaultResourceGroupPrefix + slug, nil
	}
	if ciFallback {
		return CIResourceGroup, nil
	}
	gc, err := msgraph.New(cred)
	if err != nil {
		return "", err
	}
	oid, upn, err := gc.SignedInUserObjectID(ctx)
	if err != nil {
		return "", fmt.Errorf("derive alias from signed-in user (run `az login`): %w", err)
	}
	alias := DeriveAlias(oid, upn)
	if alias == "" {
		return "", fmt.Errorf("could not derive a non-empty alias from oid=%s upn=%q; pass --alias or --resource-group", oid, upn)
	}
	return DefaultResourceGroupPrefix + alias, nil
}
