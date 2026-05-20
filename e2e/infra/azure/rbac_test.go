package azure

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func TestIsAuthorizationFailed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"auth failed code", &azcore.ResponseError{ErrorCode: "AuthorizationFailed"}, true},
		{"wrapped auth failed code", fmtWrap{inner: &azcore.ResponseError{ErrorCode: "AuthorizationFailed"}}, true},
		{"other code", &azcore.ResponseError{ErrorCode: "NoSuchThing"}, false},
		{"status only", &azcore.ResponseError{StatusCode: http.StatusForbidden}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAuthorizationFailed(tc.err); got != tc.want {
				t.Errorf("isAuthorizationFailed(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsRoleAlreadyAssigned(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"RoleAssignmentExists", &azcore.ResponseError{ErrorCode: "RoleAssignmentExists"}, true},
		{"RoleAssignmentAlreadyExists", &azcore.ResponseError{ErrorCode: "RoleAssignmentAlreadyExists"}, true},
		{"wrapped RoleAssignmentExists", fmtWrap{inner: &azcore.ResponseError{ErrorCode: "RoleAssignmentExists"}}, true},
		{"other code", &azcore.ResponseError{ErrorCode: "NoSuchThing"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRoleAlreadyAssigned(tc.err); got != tc.want {
				t.Errorf("isRoleAlreadyAssigned(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type fmtWrap struct{ inner error }

func (w fmtWrap) Error() string { return "wrapped: " + w.inner.Error() }
func (w fmtWrap) Unwrap() error { return w.inner }
