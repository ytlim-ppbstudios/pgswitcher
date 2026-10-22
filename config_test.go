package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testEnv(extra map[string]string) (func(string) string, []string) {
	env := map[string]string{
		"PGSW_TARGETS":        "v2=old.example.com,v3=new.example.com:6543",
		"PGSW_ACTIVE_TARGET":  "v2",
		"PGSW_ADMIN_PASSWORD": `pa"ss`,
		"PGSW_USERLIST":       `"app" "secret"`,
	}
	for k, v := range extra {
		env[k] = v
	}
	var environ []string
	for k, v := range env {
		environ = append(environ, k+"="+v)
	}
	return func(k string) string { return env[k] }, environ
}

func TestLoadConfig(t *testing.T) {
	getenv, environ := testEnv(map[string]string{"PGBOUNCER_POOL_MODE": "transaction"})
	c, err := LoadConfig(getenv, environ)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Targets["v3"]; got != (Target{Host: "new.example.com", Port: 6543}) {
		t.Errorf("v3 = %+v", got)
	}
	if got := c.Targets["v2"].Port; got != 5432 {
		t.Errorf("v2 port = %d", got)
	}
	if got := c.Settings["pool_mode"]; got != "transaction" {
		t.Errorf("pool_mode = %q", got)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown active target": {"PGSW_ACTIVE_TARGET": "v9"},
		"bad host":              {"PGSW_TARGETS": "v2=bad host"},
		"locked setting":        {"PGBOUNCER_LISTEN_ADDR": "1.2.3.4"},
		"newline in value":      {"PGBOUNCER_POOL_MODE": "session\nadmin_users = x"},
		"half tls":              {"PGSW_TLS_KEY": "k"},
		"no admin password":     {"PGSW_ADMIN_PASSWORD": ""},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			getenv, environ := testEnv(extra)
			if _, err := LoadConfig(getenv, environ); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRender(t *testing.T) {
	getenv, environ := testEnv(nil)
	c, err := LoadConfig(getenv, environ)
	if err != nil {
		t.Fatal(err)
	}
	c.Dir = t.TempDir()

	ini := c.RenderIni()
	for _, want := range []string{"listen_port = 5432", "pool_mode = session", "%include " + filepath.Join(c.Dir, "databases.ini")} {
		if !strings.Contains(ini, want) {
			t.Errorf("ini missing %q:\n%s", want, ini)
		}
	}
	if strings.Contains(ini, "client_tls") {
		t.Error("client TLS rendered without key")
	}

	if got := c.RenderDatabases(); !strings.Contains(got, "* = host=old.example.com port=5432") {
		t.Errorf("databases:\n%s", got)
	}
	c.ActiveTarget = "v3"
	if got := c.RenderDatabases(); !strings.Contains(got, "* = host=new.example.com port=6543") {
		t.Errorf("databases after switch:\n%s", got)
	}

	if got, want := c.RenderUserlist(), "\"app\" \"secret\"\n\"pgbouncer_admin\" \"pa\"\"ss\"\n"; got != want {
		t.Errorf("userlist = %q, want %q", got, want)
	}

	if err := c.WriteAll(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(c.Dir, "userlist.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("userlist mode = %v", info.Mode().Perm())
	}
}
