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
	if s[0] != (Server{"web1", "deploy@203.0.113.10", "22"}) {
		t.Fatalf("s[0] = %+v", s[0])
	}
	if s[1] != (Server{"db", "admin@db.internal", "2222"}) {
		t.Fatalf("s[1] = %+v", s[1])
	}
}
