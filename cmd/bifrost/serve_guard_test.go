package main

import (
	"os"
	"strings"
	"testing"
)

// serve.go must resolve inputs and hand them to app.New; it must not build
// the api.Server or the handler itself. If it did, the inproc requirement
// target (which also calls app.New) would test different wiring than
// production runs — the exact gap the requirement framework exists to close.
func TestServeDelegatesWiringToAppNew(t *testing.T) {
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, forbidden := range []string{"api.NewHandler(", "&api.Server{", "controller.RunReconciler(", "controller.RunPoolReconciler("} {
		if strings.Contains(s, forbidden) {
			t.Errorf("serve.go contains %q; that wiring belongs in internal/app", forbidden)
		}
	}
	if !strings.Contains(s, "app.New(") {
		t.Error("serve.go does not call app.New")
	}
}
