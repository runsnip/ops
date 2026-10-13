package graph

import (
	"strings"
	"testing"
)

func TestCaddyRoutesFollowSitesHandlesSnippetsAndAsk(t *testing.T) {
	text := `{
	on_demand_tls {
		ask http://gate:8080/check
	}
}
(proxy_api) {
	reverse_proxy api:4000
}
{$DOMAIN}, www.{$DOMAIN} {
	@static path /assets/*
	handle @static {
		reverse_proxy cdn:80
	}
	handle /api/* {
		import proxy_api
	}
	reverse_proxy web:3000 # the rest
}
https:// {
	reverse_proxy {
		to web:3000
	}
}`
	routes, sites := CaddyRoutes(text, map[string]string{"DOMAIN": "shop.test"})
	got := []string{}
	for _, r := range routes {
		got = append(got, strings.Join(r.Hosts, ",")+"|"+r.Path+"|"+r.Target)
	}
	want := []string{"|tls ask|http://gate:8080/check", "shop.test,www.shop.test|/assets/*|cdn:80", "shop.test,www.shop.test|/api/*|api:4000", "shop.test,www.shop.test||web:3000", "*||web:3000"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("routes:\n%s", strings.Join(got, "\n"))
	}
	if len(sites) != 2 || sites[0][0] != "shop.test" {
		t.Fatal(sites)
	}
}

func TestNginxRoutesFollowUpstreamsSetAndMap(t *testing.T) {
	text := `
map $uri $api {
  ~^/api/auth(/|$)  auth:4010;
  ~^/api/plan(/|$)  plan:4040;
}
upstream front { server web:3000 resolve; }
server {
  listen 80;
  server_name ${DOMAIN};
  location /api/ { proxy_pass http://$api; }
  location ^~ /files/ {
    set $files http://files:4030;
    proxy_pass $files;
  }
  location / { proxy_pass http://front; }
}`
	routes := NginxRoutes(text, map[string]string{"DOMAIN": "shop.test"})
	got := []string{}
	for _, r := range routes {
		got = append(got, strings.Join(r.Hosts, ",")+"|"+r.Path+"|"+r.Target)
	}
	want := []string{"shop.test|^/api/auth(/|$)|http://auth:4010", "shop.test|^/api/plan(/|$)|http://plan:4040", "shop.test|/files/|http://files:4030", "shop.test|/|web:3000"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("routes:\n%s", strings.Join(got, "\n"))
	}
}

func TestRegistrableDomains(t *testing.T) {
	for in, want := range map[string]string{"api.github.com": "github.com", "a.b.co.uk": "b.co.uk", "x.y.com.vn": "y.com.vn",
		"10.0.0.1": "10.0.0.1", "example.net": "example.net", "acc.r2.cloudflarestorage.com": "cloudflarestorage.com"} {
		if got := registrable(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}
