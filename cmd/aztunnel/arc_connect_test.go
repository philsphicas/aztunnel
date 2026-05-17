package main

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/philsphicas/aztunnel/internal/arc"
)

// TestIsHybridConnectivitySetupErr guards the classifier that decides
// whether to enable the first-connection explanatory logging. Both
// `arc connect` and `arc port-forward` call this on the initial
// GetRelayCredentials error; misclassifying it would silently disable
// (or incorrectly enable) the UX path covered by the rest of this PR.
func TestIsHybridConnectivitySetupErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"non-ARM error", errors.New("boom"), false},
		{"ARM 404 NotFound (endpoint absent)", &arc.ARMError{StatusCode: http.StatusNotFound, Body: []byte("ResourceNotFound")}, true},
		{"ARM 412 PreconditionFailed (service config missing)", &arc.ARMError{StatusCode: http.StatusPreconditionFailed, Body: []byte("PreconditionFailed")}, true},
		{"ARM 401 Unauthorized", &arc.ARMError{StatusCode: http.StatusUnauthorized, Body: []byte("Unauthorized")}, false},
		{"ARM 403 Forbidden", &arc.ARMError{StatusCode: http.StatusForbidden, Body: []byte("Forbidden")}, false},
		{"ARM 500 InternalServerError", &arc.ARMError{StatusCode: http.StatusInternalServerError, Body: []byte("InternalServerError")}, false},
		{"wrapped ARM 404", fmt.Errorf("get credentials: %w", &arc.ARMError{StatusCode: http.StatusNotFound, Body: []byte("ResourceNotFound")}), true},
		{"wrapped ARM 412", fmt.Errorf("ensure hybrid: %w", &arc.ARMError{StatusCode: http.StatusPreconditionFailed, Body: []byte("PreconditionFailed")}), true},
		{"wrapped ARM 500", fmt.Errorf("get credentials: %w", &arc.ARMError{StatusCode: http.StatusInternalServerError, Body: []byte("oops")}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isHybridConnectivitySetupErr(tt.err)
			if got != tt.want {
				t.Errorf("isHybridConnectivitySetupErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
