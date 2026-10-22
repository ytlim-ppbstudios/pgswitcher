package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const adminUser = "pgbouncer_admin"

// Target is a backend PostgreSQL endpoint that every database is routed to.
type Target struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Config is everything needed to render PgBouncer's configuration files.
type Config struct {
	Dir           string
	ListenPort    int
	Targets       map[string]Target
	ActiveTarget  string
	Settings      map[string]string
	Userlist      string
	AdminPassword string
	TLSKey        string
	TLSCert       string
}

// defaultSettings are applied before PGBOUNCER_* overrides.
var defaultSettings = map[string]string{
	"pool_mode":                 "session",
	"max_client_conn":           "5000",
	"default_pool_size":         "1000",
	"max_db_connections":        "0",
	"ignore_startup_parameters": "extra_float_digits",
	"server_tls_sslmode":        "require",
	"server_connect_timeout":    "5",
	"server_login_retry":        "1",
	"query_wait_timeout":        "120",
	"log_connections":           "0",
	"log_disconnections":        "0",
	"log_pooler_errors":         "1",
	"stats_period":              "60",
}

// runtimeSettings can be changed through the API and applied with RELOAD.
// Settings outside this list (listen address, auth, TLS files) need a restart or are managed by pgswitcher.
var runtimeSettings = map[string]bool{
	"pool_mode":                 true,
	"max_client_conn":           true,
	"default_pool_size":         true,
	"min_pool_size":             true,
	"reserve_pool_size":         true,
	"reserve_pool_timeout":      true,
	"max_db_connections":        true,
	"max_user_connections":      true,
	"server_idle_timeout":       true,
	"server_lifetime":           true,
	"server_connect_timeout":    true,
	"server_login_retry":        true,
	"server_tls_sslmode":        true,
	"query_timeout":             true,
	"query_wait_timeout":        true,
	"client_idle_timeout":       true,
	"idle_transaction_timeout":  true,
	"ignore_startup_parameters": true,
	"log_connections":           true,
	"log_disconnections":        true,
	"log_pooler_errors":         true,
	"stats_period":              true,
	"verbose":                   true,
}

var (
	targetNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	hostRe       = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
	settingRe    = regexp.MustCompile(`^[A-Za-z0-9_.,:/ -]*$`)
)

// LoadConfig reads the configuration from environment variables.
func LoadConfig(getenv func(string) string, environ []string) (*Config, error) {
	c := &Config{
		Dir:           orDefault(getenv("PGSW_CONFIG_DIR"), "/etc/pgbouncer"),
		ActiveTarget:  getenv("PGSW_ACTIVE_TARGET"),
		Userlist:      getenv("PGSW_USERLIST"),
		AdminPassword: getenv("PGSW_ADMIN_PASSWORD"),
		TLSKey:        getenv("PGSW_TLS_KEY"),
		TLSCert:       getenv("PGSW_TLS_CERT"),
		Settings:      map[string]string{},
	}

	port, err := strconv.Atoi(orDefault(getenv("PGSW_PGBOUNCER_PORT"), "5432"))
	if err != nil {
		return nil, fmt.Errorf("PGSW_PGBOUNCER_PORT: %w", err)
	}
	c.ListenPort = port

	c.Targets, err = parseTargets(getenv("PGSW_TARGETS"))
	if err != nil {
		return nil, fmt.Errorf("PGSW_TARGETS: %w", err)
	}
	if _, ok := c.Targets[c.ActiveTarget]; !ok {
		return nil, fmt.Errorf("PGSW_ACTIVE_TARGET %q is not in PGSW_TARGETS", c.ActiveTarget)
	}
	if c.AdminPassword == "" {
		return nil, fmt.Errorf("PGSW_ADMIN_PASSWORD is required")
	}
	if (c.TLSKey == "") != (c.TLSCert == "") {
		return nil, fmt.Errorf("PGSW_TLS_KEY and PGSW_TLS_CERT must be set together")
	}

	for k, v := range defaultSettings {
		c.Settings[k] = v
	}
	// PGBOUNCER_POOL_MODE=transaction -> pool_mode = transaction
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, "PGBOUNCER_") {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(k, "PGBOUNCER_"))
		if err := validateSetting(name, v); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		c.Settings[name] = v
	}
	return c, nil
}

// parseTargets parses "v2=host-a:5432,v3=host-b" (port defaults to 5432).
func parseTargets(s string) (map[string]Target, error) {
	targets := map[string]Target{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, addr, ok := strings.Cut(item, "=")
		if !ok {
			return nil, fmt.Errorf("%q: want name=host[:port]", item)
		}
		host, portStr, hasPort := strings.Cut(addr, ":")
		port := 5432
		if hasPort {
			p, err := strconv.Atoi(portStr)
			if err != nil {
				return nil, fmt.Errorf("%q: invalid port", item)
			}
			port = p
		}
		t := Target{Host: host, Port: port}
		if err := validateTarget(name, t); err != nil {
			return nil, err
		}
		targets[name] = t
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("at least one target is required")
	}
	return targets, nil
}

func validateTarget(name string, t Target) error {
	if !targetNameRe.MatchString(name) {
		return fmt.Errorf("invalid target name %q", name)
	}
	if !hostRe.MatchString(t.Host) {
		return fmt.Errorf("invalid host %q", t.Host)
	}
	if t.Port < 1 || t.Port > 65535 {
		return fmt.Errorf("invalid port %d", t.Port)
	}
	return nil
}

func validateSetting(name, value string) error {
	if !runtimeSettings[name] {
		return fmt.Errorf("setting %q cannot be changed", name)
	}
	if !settingRe.MatchString(value) {
		return fmt.Errorf("invalid value for %q", name)
	}
	return nil
}

func (c *Config) path(name string) string { return filepath.Join(c.Dir, name) }

// IniPath is the main config file passed to pgbouncer.
func (c *Config) IniPath() string { return c.path("pgbouncer.ini") }

// RenderIni renders pgbouncer.ini. The [databases] section lives in databases.ini so a switch only rewrites that file.
func (c *Config) RenderIni() string {
	var b strings.Builder
	b.WriteString("; generated by pgswitcher. do not edit.\n[pgbouncer]\n")
	fmt.Fprintf(&b, "listen_addr = 0.0.0.0\nlisten_port = %d\nunix_socket_dir =\n", c.ListenPort)
	fmt.Fprintf(&b, "auth_type = scram-sha-256\nauth_file = %s\n", c.path("userlist.txt"))
	fmt.Fprintf(&b, "admin_users = %s\nstats_users = %s\n", adminUser, adminUser)
	if c.TLSKey != "" {
		fmt.Fprintf(&b, "client_tls_sslmode = prefer\nclient_tls_key_file = %s\nclient_tls_cert_file = %s\n",
			c.path("client.key"), c.path("client.crt"))
	}

	keys := make([]string, 0, len(c.Settings))
	for k := range c.Settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, c.Settings[k])
	}

	fmt.Fprintf(&b, "\n%%include %s\n", c.path("databases.ini"))
	return b.String()
}

// RenderDatabases routes every database name to the active target.
func (c *Config) RenderDatabases() string {
	t := c.Targets[c.ActiveTarget]
	return fmt.Sprintf("; generated by pgswitcher. active target: %s\n[databases]\n* = host=%s port=%d\n",
		c.ActiveTarget, t.Host, t.Port)
}

// RenderUserlist appends the admin user to the given userlist.
func (c *Config) RenderUserlist() string {
	var b strings.Builder
	if u := strings.TrimSpace(c.Userlist); u != "" {
		b.WriteString(u)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%s %s\n", quoteUserlist(adminUser), quoteUserlist(c.AdminPassword))
	return b.String()
}

// quoteUserlist quotes a userlist.txt field. A literal quote is written as two quotes.
func quoteUserlist(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// WriteAll writes every file pgbouncer needs.
func (c *Config) WriteAll() error {
	if err := writeFileAtomic(c.path("userlist.txt"), c.RenderUserlist()); err != nil {
		return err
	}
	if c.TLSKey != "" {
		if err := writeFileAtomic(c.path("client.key"), c.TLSKey+"\n"); err != nil {
			return err
		}
		if err := writeFileAtomic(c.path("client.crt"), c.TLSCert+"\n"); err != nil {
			return err
		}
	}
	if err := writeFileAtomic(c.path("databases.ini"), c.RenderDatabases()); err != nil {
		return err
	}
	return writeFileAtomic(c.IniPath(), c.RenderIni())
}

func writeFileAtomic(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
