// Package shield is the one place runsnip meets an optional request shield: the Caddy snippet that makes every site
// ask a forward-auth endpoint before anything else, behind an opt-in .env switch. runsnip never installs, configures,
// runs or inspects the shield itself — its operator runs it; runsnip only wires Caddy to it (as stack.yaml's shield
// block describes it) and checks that Caddy's route to it answers.
package shield

import "strings"

// CaddySnippet is the shield's Caddy snippet under the names the stack standard's Caddyfile imports: (shield_on) asks
// addr+path with Caddy's forward_auth (passing on copyHeaders of its answer), (shield_off) is empty.
func CaddySnippet(addr, path string, copyHeaders []string) string {
	if path == "" {
		path = "/check"
	}
	copy := ""
	if len(copyHeaders) > 0 {
		copy = "\n\t\tcopy_headers " + strings.Join(copyHeaders, " ")
	}
	return `(shield_on) {
	forward_auth ` + addr + ` {
		uri ` + path + `
		header_up X-Forwarded-For "{http.request.header.X-Forwarded-For}, {remote_host}"` + copy + `
	}
}

(shield_off) {
}
`
}
