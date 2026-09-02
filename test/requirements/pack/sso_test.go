package pack

import (
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

// The dashboard learns its OIDC client id and issuer at runtime from
// /config.json, rendered by the chart. Before this the values were compiled
// into the image, and one published image could only ever talk to one
// deployment (Grace, 2026-09-02: the SSO button sent the browser to a stale
// realm on localhost).
func TestDashboardRuntimeSSOConfig(t *testing.T) {
	req.Covers(t, 3, "the dashboard's OIDC client id and issuer are deployment configuration served at runtime, not compiled into the image")

	out := Render(t, "image.tag=sha-test", "ui.enabled=true",
		"auth.mode=oidc", "auth.oidc.issuer=https://kc.example/realms/nebari",
		"nebariApp.ui.enabled=true", "nebariApp.ui.hostname=ui.example",
		"nebariApp.ui.auth.enabled=true", "nebariApp.ui.auth.provisionClient=true",
		"nebariApp.ui.auth.spaClient.enabled=true", "nebariApp.ui.auth.redirectURI=/auth/callback")

	mustContain(t, out, `"ssoClientId":"default-bifrost-ui-spa"`, "the client id must be the SPA client the operator provisions for the UI NebariApp (<namespace>-<nebariapp>-spa)")
	mustContain(t, out, `"issuer":"https://kc.example/realms/nebari"`, "the issuer must follow auth.oidc.issuer")
	mustContain(t, out, "location = /config.json", "nginx must serve the runtime config")
	mustContain(t, out, "alias /etc/bifrost-ui/config.json", "the served file must be the mounted ConfigMap key")
	mustContain(t, out, "mountPath: /etc/bifrost-ui/config.json", "ui.yaml must mount config.json where nginx reads it")
	mustContain(t, out, `redirectURI: "/auth/callback"`, "the SPA's callback path must be what the operator registers on the client")

	// Local auth may run beside OIDC: bfr_ PATs and the extension's dev
	// fallback need it even when users sign in with SSO.
	both := Render(t, "image.tag=sha-test", "auth.mode=oidc", "auth.oidc.issuer=https://kc.example/realms/nebari",
		"auth.local.enabled=true", "auth.local.existingSecret=s")
	mustContain(t, both, "--auth-config=/etc/bifrost/auth/auth.json", "OIDC must stay on")
	mustContain(t, both, "--local-auth", "auth.local.enabled must add --local-auth beside --auth-config")
	mustContain(t, both, "name: BIFROST_LOCAL_ADMIN_PASSWORD", "the local admin password must be wired when local auth is on")

	// An explicit override wins over the derivation.
	explicit := Render(t, "image.tag=sha-test", "ui.enabled=true", "ui.sso.clientId=my-spa", "ui.sso.issuer=https://idp/realms/x")
	mustContain(t, explicit, `"ssoClientId":"my-spa"`, "ui.sso.clientId must override the derived id")
	mustContain(t, explicit, `"issuer":"https://idp/realms/x"`, "ui.sso.issuer must override the derived issuer")
}
