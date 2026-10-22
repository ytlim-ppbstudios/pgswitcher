package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Connstring 은 PgBouncer [databases] 항목의 값. 예: "host=db.example.com port=5432 pool_size=50".
// 입력 순서를 그대로 유지한다.
type Connstring []ConnParam

type ConnParam struct {
	Key   string
	Value string
}

// connKeys 는 PgBouncer 가 [databases] 항목에서 받는 키.
var connKeys = map[string]bool{
	"dbname":                    true,
	"host":                      true,
	"port":                      true,
	"user":                      true,
	"password":                  true,
	"auth_user":                 true,
	"auth_query":                true,
	"auth_dbname":               true,
	"pool_size":                 true,
	"min_pool_size":             true,
	"reserve_pool_size":         true,
	"connect_query":             true,
	"pool_mode":                 true,
	"load_balance_hosts":        true,
	"max_db_connections":        true,
	"max_db_client_connections": true,
	"server_lifetime":           true,
	"client_encoding":           true,
	"datestyle":                 true,
	"timezone":                  true,
	"application_name":          true,
}

// ParseConnstring 은 PgBouncer 연결 문자열을 파싱한다. 공백으로 구분한 key=value 이고,
// 값은 작은따옴표로 감쌀 수 있으며 값 안의 작은따옴표는 두 번 쓴다. host 는 필수.
func ParseConnstring(s string) (Connstring, error) {
	if strings.ContainsAny(s, "\r\n\x00") {
		return nil, fmt.Errorf("must be a single line")
	}
	var cs Connstring
	seen := map[string]bool{}
	rest := strings.TrimSpace(s)
	for rest != "" {
		key, after, ok := strings.Cut(rest, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.Contains(key, " ") {
			return nil, fmt.Errorf("want key=value pairs near %q", rest)
		}
		if !connKeys[key] {
			return nil, fmt.Errorf("unknown key %q", key)
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		seen[key] = true

		value, remain, err := cutValue(strings.TrimLeft(after, " "))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		cs = append(cs, ConnParam{Key: key, Value: value})
		rest = strings.TrimSpace(remain)
	}

	if cs.Get("host") == "" {
		return nil, fmt.Errorf("host is required")
	}
	if p := cs.Get("port"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("invalid port %q", p)
		}
	}
	return cs, nil
}

func cutValue(s string) (value, rest string, err error) {
	if !strings.HasPrefix(s, "'") {
		value, rest, _ = strings.Cut(s, " ")
		if value == "" {
			return "", "", fmt.Errorf("empty value")
		}
		if strings.Contains(value, "'") {
			return "", "", fmt.Errorf("unquoted value must not contain a quote")
		}
		return value, rest, nil
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		if b.Len() == 0 {
			return "", "", fmt.Errorf("empty value")
		}
		rest = s[i+1:]
		if rest != "" && rest[0] != ' ' {
			return "", "", fmt.Errorf("missing space after quoted value")
		}
		return b.String(), rest, nil
	}
	return "", "", fmt.Errorf("unterminated quote")
}

func (cs Connstring) Get(key string) string {
	for _, p := range cs {
		if p.Key == key {
			return p.Value
		}
	}
	return ""
}

// String 은 pgbouncer.ini 에 쓸 연결 문자열을 만든다. 필요한 값만 따옴표로 감싼다.
func (cs Connstring) String() string {
	return cs.render(false)
}

// Redacted 는 password 를 가린 String. API 응답과 로그용.
func (cs Connstring) Redacted() string {
	return cs.render(true)
}

func (cs Connstring) render(redact bool) string {
	parts := make([]string, 0, len(cs))
	for _, p := range cs {
		v := p.Value
		if redact && p.Key == "password" {
			v = "****"
		}
		if strings.ContainsAny(v, " '") {
			v = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
		parts = append(parts, p.Key+"="+v)
	}
	return strings.Join(parts, " ")
}

func (cs Connstring) MarshalJSON() ([]byte, error) {
	return json.Marshal(cs.Redacted())
}
