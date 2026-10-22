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

// Config 는 PgBouncer 설정 파일을 만드는 데 필요한 값 전체.
type Config struct {
	Dir           string
	ListenPort    int
	Backend       Connstring
	Settings      map[string]string
	Userlist      string
	AdminPassword string
	TLSKey        string
	TLSCert       string
}

// defaultSettings 는 PGBOUNCER_* 환경변수로 덮어쓰기 전 기본값.
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

// runtimeSettings 는 API 로 바꾸고 RELOAD 로 반영할 수 있는 설정.
// 여기 없는 설정(리슨 주소, 인증, TLS 파일)은 재시작이 필요하거나 pgswitcher 가 직접 관리한다.
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
	settingRe = regexp.MustCompile(`^[A-Za-z0-9_.,:/ -]*$`)
)

// LoadConfig 는 환경변수에서 설정을 읽는다.
func LoadConfig(getenv func(string) string, environ []string) (*Config, error) {
	c := &Config{
		Dir:           orDefault(getenv("PGSW_CONFIG_DIR"), "/etc/pgbouncer"),
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

	c.Backend, err = ParseConnstring(getenv("PGSW_BACKEND"))
	if err != nil {
		return nil, fmt.Errorf("PGSW_BACKEND: %w", err)
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
	// 예: PGBOUNCER_POOL_MODE=transaction -> pool_mode = transaction
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

// IniPath 는 pgbouncer 에 넘기는 메인 설정 파일 경로.
func (c *Config) IniPath() string { return c.path("pgbouncer.ini") }

// RenderIni 는 pgbouncer.ini 를 만든다. [databases] 는 databases.ini 로 분리해서 백엔드 교체 때 그 파일만 다시 쓴다.
func (c *Config) RenderIni() string {
	var b strings.Builder
	b.WriteString("; pgswitcher 가 생성한 파일. 직접 수정하지 말 것.\n[pgbouncer]\n")
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

// RenderDatabases 는 모든 DB 이름을 백엔드 하나로 보내는 [databases] 를 만든다.
func (c *Config) RenderDatabases() string {
	return fmt.Sprintf("; pgswitcher 가 생성한 파일. 직접 수정하지 말 것.\n[databases]\n* = %s\n", c.Backend)
}

// RenderUserlist 는 받은 userlist 에 관리 계정을 덧붙인다.
func (c *Config) RenderUserlist() string {
	var b strings.Builder
	if u := strings.TrimSpace(c.Userlist); u != "" {
		b.WriteString(u)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%s %s\n", quoteUserlist(adminUser), quoteUserlist(c.AdminPassword))
	return b.String()
}

// quoteUserlist 는 userlist.txt 필드를 따옴표로 감싼다. 값 안의 따옴표는 두 번 쓴다.
func quoteUserlist(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// WriteAll 은 pgbouncer 가 쓰는 파일을 모두 쓴다.
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
