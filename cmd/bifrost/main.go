// Command bifrost is the control-plane binary (Wave 1 Task 15): pure
// composition over internal/{core,policy,controller,provision,auth,api} —
// no domain logic lives here (COVERAGE_EXCLUDE, scripts/coverage-gate.sh).
//
// Subcommands mirror mobula-cli's crates/mobula-cli/src/main.rs 1:1 in
// shape (serve/login/logout/token/exchange), scoped to what Wave 1 landed:
// store selection (memory/sqlite/postgres), the reconcile + pool-reconcile
// loops, the live KubeRay/Kueue provisioner, the API server + federating
// gateway, JSON registry loading (see registry.go for why JSON, not
// mobula's TOML), and the OIDC device-code/client-credentials/token-
// exchange flows. Deliberately NOT ported this wave (see the task
// report): --demo (mock provisioner), --policy (governance/pricing file),
// --metering-interval — all Wave 3 per the plan's scope-outs.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// buildVersion is the CLI's reported version; overridable at link time
// with -ldflags "-X main.buildVersion=...". Kept in sync by convention
// with internal/api's version var (both track the vendored contract's
// info.version until a real release process exists).
var buildVersion = "0.0.1"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "bifrost:", err)
		os.Exit(1)
	}
}

// newRootCmd builds the bifrost command tree. Product identity: the
// binary and every user-visible string here say "bifrost" (Global
// Constraints), never mobula.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "bifrost",
		Short:         "FOSS control plane for Ray clusters",
		Version:       buildVersion,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCmd())
	root.AddCommand(newLoginCmd())
	root.AddCommand(newLogoutCmd())
	root.AddCommand(newTokenCmd())
	root.AddCommand(newExchangeCmd())
	return root
}

// envOr returns the environment variable's value, or def when it is unset
// or empty. Used for flag defaults that mirror mobula-cli's clap `env =`
// attributes (e.g. BIFROST_SERVER, BIFROST_CLIENT_SECRET).
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
