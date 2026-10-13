package dbx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runsnip/ops/internal/run"
)

// SidecarImage is the image the sidecar tunnel runs per connection (socat as its entrypoint).
const SidecarImage = "alpine/socat:1.8.0.3"

// TunnelOptions are `db tunnel`'s.
type TunnelOptions struct {
	Select    Select
	Docker    Docker
	LocalPort int // 0: the engine's port + 10000 when free, else any
	// Via is "ip" (an SSH local forward to the container's address on its network) or "sidecar" (each connection
	// carried over SSH to a short-lived socat container sharing the database container's network namespace — no
	// address routing, no published port, works with internal networks and without sshd forwarding).
	Via   string
	Image string
	Out   io.Writer
	Run   run.Runner
	// Ready is called once the local port answers (tests).
	Ready func(addr string)
}

// Tunnel opens 127.0.0.1:<port> on this machine to the database on the server, until ctx ends (Ctrl-C).
func Tunnel(ctx context.Context, s *SSH, o TunnelOptions) error {
	out := o.Out
	say(out, "finding the database on %s", s.Target)
	t, err := Find(ctx, s, o.Docker, o.Select)
	if err != nil {
		return err
	}
	raw, err := sh(ctx, s, o.Docker.Exec(t.Container.ID, InfoScript(t.Kind)))
	if err != nil {
		return fmt.Errorf("asking %s: %v", t.Container.Name, err)
	}
	info, err := ParseInfo(raw)
	if err != nil {
		return err
	}
	port := o.LocalPort
	if port == 0 {
		port = t.Kind.Port() + 10000
		if l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
			l.Close()
		} else if port, err = freePort(); err != nil {
			return err
		}
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	via := o.Via
	if via == "" {
		via = "ip"
	}
	switch via {
	case "ip":
		ips, err := sh(ctx, s, o.Docker.Cmd("inspect", "-f", `{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}`, t.Container.ID))
		if err != nil {
			return err
		}
		f := strings.Fields(ips)
		if len(f) == 0 {
			return fmt.Errorf("%s has no address on a docker network: try --via sidecar", t.Container.Name)
		}
		remote := f[0] + ":" + strconv.Itoa(t.Kind.Port())
		s.Close() // the forward is its own connection, ended by Ctrl-C
		a := append(s.base(), "-N", "-T", "-o", "ExitOnForwardFailure=yes", "-L", addr+":"+remote, "--", s.Target)
		fwd, stop := context.WithCancel(ctx)
		defer stop()
		done := make(chan error, 1)
		go func() {
			done <- o.Run.Run(fwd, run.Call{Name: s.Cmd[0], Args: a, Attach: true, Stdout: os.Stderr})
		}()
		// Every way out below ends the ssh process: no forward is left behind.
		quit := func(err error) error {
			stop()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
			}
			return err
		}
		if err := waitListen(ctx, addr, done); err != nil {
			return quit(err)
		}
		if err := Probe(addr, t.Kind); err != nil {
			return quit(fmt.Errorf("the forward is up but %s does not answer through it (%v): the server cannot reach that address, or its sshd forbids forwarding — try --via sidecar", remote, err))
		}
		printTunnel(out, addr, port, t, info, s.Target, remote, o)
		err = <-done
		if ctx.Err() != nil {
			fmt.Fprintln(out, "\n  tunnel closed")
			return nil
		}
		return fmt.Errorf("ssh ended: %v", err)
	case "sidecar":
		img := o.Image
		if img == "" {
			img = SidecarImage
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		defer l.Close()
		cmd := o.Docker.Cmd("run", "--rm", "-i", "--network", "container:"+t.Container.ID, "--log-driver", "none", img,
			"STDIO", "TCP:127.0.0.1:"+strconv.Itoa(t.Kind.Port()))
		go func() { <-ctx.Done(); l.Close() }()
		probed := make(chan error, 1)
		go func() { probed <- Probe(addr, t.Kind) }()
		go func() {
			if err := <-probed; err != nil {
				fmt.Fprintf(out, "  ✗ the database does not answer through the sidecar: %v\n", err)
			}
		}()
		printTunnel(out, addr, port, t, info, s.Target, "a socat sidecar per connection", o)
		var wg sync.WaitGroup
		for {
			c, err := l.Accept()
			if err != nil {
				wg.Wait()
				if ctx.Err() != nil {
					fmt.Fprintln(out, "\n  tunnel closed")
					return nil
				}
				return err
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				var errb strings.Builder
				if err := s.Sh(ctx, cmd, c, c, &errb); err != nil && ctx.Err() == nil {
					fmt.Fprintf(out, "  a connection ended: %v %s\n", err, lastLines(strings.TrimSpace(errb.String()), 2))
				}
			}()
		}
	}
	return fmt.Errorf("--via %s: ip or sidecar", via)
}

// Probe checks the database answers at addr: PostgreSQL to an SSLRequest (with 'S' or 'N'), MySQL with its greeting.
func Probe(addr string, k Kind) error {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if !k.IsMySQL() {
		if _, err := c.Write([]byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}); err != nil {
			return err
		}
		b := make([]byte, 1)
		if _, err := io.ReadFull(c, b); err != nil {
			return fmt.Errorf("no answer: %v", err)
		}
		if b[0] != 'S' && b[0] != 'N' {
			return fmt.Errorf("not PostgreSQL (answered %q)", b[0])
		}
		return nil
	}
	b := make([]byte, 5)
	if _, err := io.ReadFull(c, b); err != nil {
		return fmt.Errorf("no greeting: %v", err)
	}
	return nil
}

// waitListen waits until addr accepts a connection (the ssh forward is up), or ssh ends.
func waitListen(ctx context.Context, addr string, done chan error) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("ssh ended")
			}
			return fmt.Errorf("the forward did not start: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return nil
		}
	}
	return fmt.Errorf("%s did not open in 60 s", addr)
}

func printTunnel(out io.Writer, addr string, port int, t Target, info Info, server, remote string, o TunnelOptions) {
	fmt.Fprintf(out, "\n✓ %s → %s on %s (%s)\n\n", addr, t.Container.Name, server, remote)
	fmt.Fprintf(out, "  host 127.0.0.1   port %d   database %s   user %s\n", port, info.Database, info.User)
	if t.Kind.IsMySQL() {
		fmt.Fprintf(out, "  mysql -h 127.0.0.1 -P %d -u %s -p %s\n", port, info.User, info.Database)
		fmt.Fprintln(out, "  password: the server's MYSQL_PASSWORD / root password (its .env; not shown here)")
	} else {
		fmt.Fprintf(out, "  psql \"host=127.0.0.1 port=%d dbname=%s user=%s\"\n", port, info.Database, info.User)
		fmt.Fprintln(out, "  password: the server's POSTGRES_PASSWORD (its .env; not shown here)")
	}
	fmt.Fprintln(out, "  Only this machine can use it (127.0.0.1); nothing is published on the server. Ctrl-C closes it.")
	if o.Ready != nil {
		o.Ready(addr)
	}
}
