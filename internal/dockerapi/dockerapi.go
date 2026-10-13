// Package dockerapi asks the Docker Engine over its socket for what the dashboards poll — containers and their stats —
// instead of starting `docker compose ps` and `docker stats` processes every two seconds (which cost twelve times what
// runsnip itself did while a page was open).
package dockerapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Client talks to the Engine on a unix socket.
type Client struct {
	Socket string
	http   *http.Client

	mu   sync.Mutex
	prev map[string]cpuSample
}

type cpuSample struct{ total, system uint64 }

// Socket is DOCKER_HOST's unix socket, else /var/run/docker.sock.
func Socket() string {
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		return strings.TrimPrefix(h, "unix://")
	}
	return "/var/run/docker.sock"
}

// New returns a client for the socket, or nil when there is none to talk to.
func New(socket string) *Client {
	if st, err := os.Stat(socket); err != nil || st.Mode()&os.ModeSocket == 0 {
		return nil
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
	}
	return &Client{Socket: socket, http: &http.Client{Transport: tr, Timeout: 15 * time.Second}, prev: map[string]cpuSample{}}
}

func (c *Client) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("docker %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Container is one container of a compose project.
type Container struct {
	ID, Name, Service, Image, State, Status, Health string
	Ports                                           []string
}

// Containers lists a compose project's containers, stopped ones included.
func (c *Client) Containers(ctx context.Context, project string) ([]Container, error) {
	f, _ := json.Marshal(map[string][]string{"label": {"com.docker.compose.project=" + project}})
	var rows []struct {
		ID     string `json:"Id"`
		Names  []string
		Image  string
		State  string
		Status string
		Labels map[string]string
		Ports  []struct {
			PrivatePort, PublicPort int
			Type                    string
		}
	}
	if err := c.get(ctx, "/containers/json?all=1&filters="+url.QueryEscape(string(f)), &rows); err != nil {
		return nil, err
	}
	var out []Container
	for _, r := range rows {
		ct := Container{ID: r.ID, Image: r.Image, State: r.State, Status: r.Status, Service: r.Labels["com.docker.compose.service"]}
		if len(r.Names) > 0 {
			ct.Name = strings.TrimPrefix(r.Names[0], "/")
		}
		switch {
		case strings.Contains(r.Status, "(healthy)"):
			ct.Health = "healthy"
		case strings.Contains(r.Status, "(unhealthy)"):
			ct.Health = "unhealthy"
		case strings.Contains(r.Status, "(health: starting)"):
			ct.Health = "starting"
		}
		seen := map[string]bool{}
		for _, p := range r.Ports {
			if p.PublicPort > 0 {
				x := fmt.Sprintf("%d→%d/%s", p.PublicPort, p.PrivatePort, p.Type)
				if !seen[x] {
					seen[x] = true
					ct.Ports = append(ct.Ports, x)
				}
			}
		}
		out = append(out, ct)
	}
	return out, nil
}

// Stats is one container's use.
type Stats struct {
	CPUPerc      float64 // % of one core, like docker stats
	MemBytes     uint64
	NetRx, NetTx uint64
}

// Stats samples a running container once (one-shot: no wait). CPU is the share since this client's previous sample
// of the same container (0 the first time).
func (c *Client) Stats(ctx context.Context, id string) (Stats, error) {
	var r struct {
		CPU struct {
			Usage struct {
				Total uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			System uint64 `json:"system_cpu_usage"`
			Online int    `json:"online_cpus"`
		} `json:"cpu_stats"`
		Mem struct {
			Usage uint64            `json:"usage"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
		Networks map[string]struct {
			Rx uint64 `json:"rx_bytes"`
			Tx uint64 `json:"tx_bytes"`
		} `json:"networks"`
	}
	if err := c.get(ctx, "/containers/"+id+"/stats?stream=false&one-shot=true", &r); err != nil {
		return Stats{}, err
	}
	var s Stats
	c.mu.Lock()
	p, ok := c.prev[id]
	c.prev[id] = cpuSample{r.CPU.Usage.Total, r.CPU.System}
	c.mu.Unlock()
	if ok && r.CPU.System > p.system && r.CPU.Usage.Total >= p.total {
		online := r.CPU.Online
		if online == 0 {
			online = 1
		}
		s.CPUPerc = float64(r.CPU.Usage.Total-p.total) / float64(r.CPU.System-p.system) * float64(online) * 100
	}
	// What `docker stats` shows: usage less the page cache that can be dropped.
	s.MemBytes = r.Mem.Usage
	if v, ok := r.Mem.Stats["inactive_file"]; ok && v < s.MemBytes {
		s.MemBytes -= v
	} else if v, ok := r.Mem.Stats["total_inactive_file"]; ok && v < s.MemBytes {
		s.MemBytes -= v
	}
	for _, n := range r.Networks {
		s.NetRx += n.Rx
		s.NetTx += n.Tx
	}
	return s, nil
}

// Created is when a container was created (for "is the config newer than the container").
func (c *Client) Inspect(ctx context.Context, id string, v any) error {
	return c.get(ctx, "/containers/"+id+"/json", v)
}
