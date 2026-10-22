package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Backend 는 PgBouncer [databases] 항목을 JSON 객체로 받은 것. 예: {"host": "db.example.com", "port": 5432}
// 값은 문자열 또는 숫자만 받고, 내부에서는 문자열로 들고 있는다.
type Backend map[string]string

// backendKeys 는 PgBouncer 가 [databases] 항목에서 받는 키.
var backendKeys = map[string]bool{
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

// ParseBackend 는 JSON 객체를 읽어 검증한다. host 는 필수.
func ParseBackend(data []byte) (Backend, error) {
	var raw map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("want a JSON object: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("want a single JSON object")
	}

	b := Backend{}
	for k, v := range raw {
		if !backendKeys[k] {
			return nil, fmt.Errorf("unknown key %q", k)
		}
		switch v := v.(type) {
		case string:
			b[k] = v
		case json.Number:
			b[k] = v.String()
		default:
			return nil, fmt.Errorf("%s: must be a string or number", k)
		}
		if b[k] == "" {
			return nil, fmt.Errorf("%s: empty value", k)
		}
		// pgbouncer.ini 한 줄에 들어가야 하므로 줄바꿈은 받지 않는다.
		if strings.ContainsAny(b[k], "\r\n\x00") {
			return nil, fmt.Errorf("%s: must be a single line", k)
		}
	}

	if b["host"] == "" {
		return nil, fmt.Errorf("host is required")
	}
	if p, ok := b["port"]; ok {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("invalid port %q", p)
		}
	}
	return b, nil
}

// keys 는 host, port 를 앞에 두고 나머지는 이름순으로 정렬한다.
func (b Backend) keys() []string {
	ks := slices.Sorted(maps.Keys(b))
	slices.SortStableFunc(ks, func(x, y string) int {
		return rank(x) - rank(y)
	})
	return ks
}

func rank(k string) int {
	switch k {
	case "host":
		return 0
	case "port":
		return 1
	}
	return 2
}

// String 은 pgbouncer.ini 에 쓸 연결 문자열을 만든다.
// 공백이나 작은따옴표가 있는 값은 작은따옴표로 감싸고, 값 안의 작은따옴표는 두 번 쓴다.
func (b Backend) String() string {
	parts := make([]string, 0, len(b))
	for _, k := range b.keys() {
		v := b[k]
		if v == "" || strings.ContainsAny(v, " '") {
			v = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, " ")
}

// Redacted 는 password 를 가린 사본. API 응답과 로그용.
func (b Backend) Redacted() Backend {
	r := maps.Clone(b)
	if _, ok := r["password"]; ok {
		r["password"] = "****"
	}
	return r
}
