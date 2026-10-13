package graph

import "strings"

// iconFor is a node's icon: a name in the page's sprite (internal/serve/web/icons.svg, made from @runsnip/icons by
// scripts/icons). Images are matched by their own name; anything unknown gets its kind's neutral icon. stack.yaml's
// services.<name>.graph.icon overrides it.
func iconFor(kind, image string) string {
	for _, m := range imageIcons {
		for _, p := range m.prefixes {
			if image == p || strings.HasPrefix(image, p+"-") || strings.HasPrefix(image, p) && len(image) > len(p) && image[len(p)] >= '0' && image[len(p)] <= '9' {
				return m.icon
			}
		}
	}
	switch kind {
	case KindHost:
		return "globe"
	case KindProxy:
		return "proxy"
	case KindData:
		return "database"
	case KindExternal:
		return "cloud"
	case KindRegistry:
		return "registry"
	}
	return "container"
}

var imageIcons = []struct {
	icon     string
	prefixes []string
}{
	{"database", []string{"postgres", "postgis", "pgvector", "timescaledb", "mysql", "mariadb", "percona", "mongo", "mongodb", "clickhouse", "cassandra", "scylla", "cockroach", "couchdb", "neo4j", "influxdb"}},
	{"cache", []string{"redis", "valkey", "keydb", "dragonfly", "memcached"}},
	{"queue", []string{"rabbitmq", "nats", "kafka", "redpanda"}},
	{"bucket", []string{"minio", "versitygw", "garage", "seaweedfs"}},
	{"search", []string{"elasticsearch", "opensearch", "meilisearch", "typesense", "qdrant", "weaviate"}},
	{"proxy", []string{"caddy", "nginx", "traefik", "haproxy", "envoy"}},
	{"node", []string{"node"}},
	{"go", []string{"golang"}},
	{"rust", []string{"rust"}},
}
