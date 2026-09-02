package pack

import (
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

// Defect 4 (docs/defects, 2026-09-02): the dashboard's nginx resolved
// `proxy_pass http://bifrost:8484` once at startup and exited when CoreDNS
// was not yet up, taking the UI down on every node reboot. The fix defers
// resolution to request time: a `resolver` directive plus a variable
// upstream. The nginx image renders /etc/nginx/templates/*.template with
// envsubst at boot and exports NGINX_LOCAL_RESOLVERS from /etc/resolv.conf,
// so the chart ships a template, not a finished conf.
func TestDashboardNginxResolvesUpstreamAtRequestTime(t *testing.T) {
	req.Covers(t, 6, "the self-serve dashboard survives a node reboot: nginx does not need DNS to start")
	req.Covers(t, 8, "deployment recovers after infrastructure failure without manual intervention")

	out := Render(t, "image.tag=sha-test", "ui.enabled=true")

	mustContain(t, out, "resolver ${NGINX_LOCAL_RESOLVERS}", "nginx must use a runtime resolver so upstream names are re-resolved")
	mustContain(t, out, "set $bifrost_upstream", "the upstream must be a variable so nginx resolves it per request, not at boot")
	mustContain(t, out, "proxy_pass $bifrost_upstream", "proxy_pass must reference the variable")
	mustNotContain(t, out, "proxy_pass http://", "a literal proxy_pass host is resolved once at startup and kills nginx if DNS is down")
	mustContain(t, out, "default.conf.template", "the conf must ship as a template so the image's envsubst renders it at boot")
	mustContain(t, out, "/etc/nginx/templates", "the template must be mounted where the image's entrypoint looks")
	mustContain(t, out, "NGINX_ENVSUBST_FILTER", "envsubst must be restricted to NGINX_* or it will clobber nginx's own $variables")
}
