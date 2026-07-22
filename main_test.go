package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvFileAndScript(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env.txt")
	os.WriteFile(p, []byte(`# comment
APP_ENV=staging
GREETING=it's a 'test'

cd /srv/app
echo ready
`), 0o600)

	vars, cmds, err := parseEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 2 || vars[0] != "APP_ENV=staging" {
		t.Fatalf("vars = %q", vars)
	}
	if len(cmds) != 2 || cmds[0] != "cd /srv/app" {
		t.Fatalf("cmds = %q", cmds)
	}

	got := buildScript(vars, cmds)
	want := `export APP_ENV='staging'; export GREETING='it'\''s a '\''test'\'''; ` +
		`cd /srv/app; echo ready; ` +
		`printf '\n== Horizon: environment applied ==\n'; ` +
		`printf 'Variables set:\n'; ` +
		`printf '  %s=%s\n' 'APP_ENV' "$APP_ENV"; ` +
		`printf '  %s=%s\n' 'GREETING' "$GREETING"; ` +
		`printf 'Commands run:\n'; ` +
		`printf '  %s\n' 'cd /srv/app'; ` +
		`printf '  %s\n' 'echo ready'; ` +
		`printf '\n'; ` +
		`exec "$SHELL" -l`
	if got != want {
		t.Fatalf("script:\n got %s\nwant %s", got, want)
	}
}

func TestLoadServers(t *testing.T) {
	baseDir = t.TempDir()
	os.WriteFile(filepath.Join(baseDir, serversFileName), []byte(`# comment
web1 deploy@203.0.113.10
db   admin@db.internal:2222
malformed-line
`), 0o600)

	s := loadServers()
	if len(s) != 2 {
		t.Fatalf("servers = %+v", s)
	}
	if s[0] != (Server{"web1", "deploy@203.0.113.10", "22", ""}) {
		t.Fatalf("s[0] = %+v", s[0])
	}
	if s[1] != (Server{"db", "admin@db.internal", "2222", ""}) {
		t.Fatalf("s[1] = %+v", s[1])
	}
}

func TestLoadServersGroups(t *testing.T) {
	baseDir = t.TempDir()
	os.WriteFile(filepath.Join(baseDir, serversFileName), []byte(`jump ops@jump.example.com
[production]
web1 deploy@203.0.113.10
[ staging ]
web2 deploy@203.0.113.11:2222
`), 0o600)

	s := loadServers()
	if len(s) != 3 {
		t.Fatalf("servers = %+v", s)
	}
	if s[0] != (Server{"jump", "ops@jump.example.com", "22", ""}) {
		t.Fatalf("s[0] = %+v", s[0])
	}
	if s[1] != (Server{"web1", "deploy@203.0.113.10", "22", "production"}) {
		t.Fatalf("s[1] = %+v", s[1])
	}
	if s[2] != (Server{"web2", "deploy@203.0.113.11", "2222", "staging"}) {
		t.Fatalf("s[2] = %+v", s[2])
	}
}

func TestInsertServerLine(t *testing.T) {
	base := "jump ops@jump.example.com\n[production]\nweb1 deploy@10.0.0.1:22\n[staging]\nweb2 deploy@10.0.0.2:22\n"

	cases := []struct{ name, group, want string }{
		{"top level goes before first group", "",
			"jump ops@jump.example.com\nnew u@h:22\n[production]\nweb1 deploy@10.0.0.1:22\n[staging]\nweb2 deploy@10.0.0.2:22\n"},
		{"existing group extends its section", "production",
			"jump ops@jump.example.com\n[production]\nweb1 deploy@10.0.0.1:22\nnew u@h:22\n[staging]\nweb2 deploy@10.0.0.2:22\n"},
		{"last group appends at end", "staging",
			"jump ops@jump.example.com\n[production]\nweb1 deploy@10.0.0.1:22\n[staging]\nweb2 deploy@10.0.0.2:22\nnew u@h:22\n"},
		{"new group appended with header", "db",
			"jump ops@jump.example.com\n[production]\nweb1 deploy@10.0.0.1:22\n[staging]\nweb2 deploy@10.0.0.2:22\n[db]\nnew u@h:22\n"},
	}
	for _, c := range cases {
		if got := insertServerLine(base, "new u@h:22", c.group); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}

	if got := insertServerLine("", "new u@h:22", ""); got != "new u@h:22\n" {
		t.Errorf("empty file: %q", got)
	}
	if got := insertServerLine("", "new u@h:22", "prod"); got != "[prod]\nnew u@h:22\n" {
		t.Errorf("empty file with group: %q", got)
	}
}

func TestParseConfig(t *testing.T) {
	if c := parseConfig(""); c.Ping || c.PingCount != 3 {
		t.Fatalf("defaults = %+v", c)
	}
	if c := parseConfig(configTemplate); c.Ping || c.PingCount != 3 {
		t.Fatalf("template = %+v", c)
	}
	c := parseConfig("# comment\nping = on\nping_count = 5\njunk line\nunknown=1\n")
	if !c.Ping || c.PingCount != 5 {
		t.Fatalf("parsed = %+v", c)
	}
	// Invalid count falls back to the default; ping stays off unless truthy.
	c = parseConfig("ping=maybe\nping_count=zero\n")
	if c.Ping || c.PingCount != 3 {
		t.Fatalf("invalid values = %+v", c)
	}
}

func TestParsePingAvg(t *testing.T) {
	mac := "--- 203.0.113.10 ping statistics ---\n" +
		"3 packets transmitted, 3 packets received, 0.0% packet loss\n" +
		"round-trip min/avg/max/stddev = 13.6/14.2/14.9/0.5 ms\n"
	linux := "--- db.internal ping statistics ---\n" +
		"3 packets transmitted, 3 received, 0% packet loss, time 2003ms\n" +
		"rtt min/avg/max/mdev = 20.1/21.0/22.0/0.5 ms\n"

	if avg, ok := parsePingAvg(mac); !ok || avg != "14.2" {
		t.Fatalf("mac avg = %q %v", avg, ok)
	}
	if avg, ok := parsePingAvg(linux); !ok || avg != "21.0" {
		t.Fatalf("linux avg = %q %v", avg, ok)
	}
	if _, ok := parsePingAvg("ping: cannot resolve nope.invalid: Unknown host\n"); ok {
		t.Fatal("expected no match for error output")
	}
}
