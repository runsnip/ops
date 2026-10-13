package main

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/runsnip/ops/internal/auth"
	"github.com/runsnip/ops/internal/host"
)

const adminUsage = `runsnip admin — who may use the web UI (runsnip serve)

  runsnip admin setup-code [--ttl 30m]   the one-time code for the first admin (only while there is none)
  runsnip admin add <name> [--ttl 30m]   a new admin, with a code to register a passkey or TOTP on the web
  runsnip admin reset <name>             drop their passkeys and TOTP, give a new enrolment code
  runsnip admin remove <name>            never the last admin who can sign in
  runsnip admin list`

func adminCmd(g globals, args []string, out io.Writer) (int, error) {
	if err := host.Ensure(g.state); err != nil {
		return 1, fmt.Errorf("runsnip's state %s: %v (run as root?)", g.state, err)
	}
	s := auth.NewStore(g.state)
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	ttl := 30 * time.Minute
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--ttl" && i+1 < len(args) {
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d <= 0 || d > 24*time.Hour {
				return 2, errors.New("--ttl: a duration up to 24h")
			}
			ttl, i = d, i+1
			continue
		}
		rest = append(rest, args[i])
	}
	note := func(event, name string) {
		_ = host.Audit(g.state, map[string]any{"event": event, "admin": name, "user": who(), "via": via()})
	}
	switch sub {
	case "setup-code":
		code, err := s.SetupCode(ttl)
		if err != nil {
			return 1, err
		}
		note("admin.setup-code", "")
		fmt.Fprintf(out, "Setup code: %s  (valid %s, once, only until the first admin exists)\nOpen the web UI and enter it to create the first admin.\n", code, ttl)
		return 0, nil
	case "add", "reset":
		if len(rest) != 1 {
			return 2, errors.New(adminUsage)
		}
		var code string
		var err error
		if sub == "add" {
			code, err = s.Add(rest[0], ttl)
		} else {
			code, err = s.Reset(rest[0], ttl)
		}
		if err != nil {
			return 1, err
		}
		note("admin."+sub, rest[0])
		fmt.Fprintf(out, "Enrolment code for %s: %s  (valid %s)\nThey open the web UI, choose \"I have an enrolment code\", and register a passkey or TOTP.\n", rest[0], code, ttl)
		return 0, nil
	case "remove":
		if len(rest) != 1 {
			return 2, errors.New(adminUsage)
		}
		if err := s.Remove(rest[0]); err != nil {
			return 1, err
		}
		note("admin.remove", rest[0])
		fmt.Fprintf(out, "%s removed (signed out on the next request)\n", rest[0])
		return 0, nil
	case "list":
		all, err := s.Admins()
		if err != nil {
			return 1, err
		}
		for _, a := range all {
			state := "can sign in"
			if !a.HasCredential() {
				state = "enrolment pending"
			}
			fmt.Fprintf(out, "%-20s %d passkey(s)  TOTP %-3s  %-18s since %s\n", a.Name, len(a.Passkeys), map[bool]string{true: "on", false: "off"}[a.TOTP != ""], state, a.Created.Format("2006-01-02"))
		}
		if len(all) == 0 {
			fmt.Fprintln(out, "no admin yet: runsnip admin setup-code")
		}
		return 0, nil
	}
	fmt.Fprintln(out, adminUsage)
	if sub == "" || sub == "help" {
		return 0, nil
	}
	return 2, fmt.Errorf("unknown: runsnip admin %s", sub)
}
