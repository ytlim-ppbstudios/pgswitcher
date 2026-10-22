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
// 키마다 JSON 타입을 검사하고, 내부에서는 pgbouncer.ini 에 쓸 문자열로 들고 있는다.
// 요청 형식은 README 의 "PUT /backend" 참고.
type Backend map[string]string

type keyKind int

const (
	kindString keyKind = iota // JSON 문자열
	kindUint                  // 0 이상의 JSON 정수
	kindPort                  // 1~65535 JSON 정수
	kindEnum                  // 정해진 값 중 하나인 JSON 문자열
)

type keySpec struct {
	kind   keyKind
	values []string // kindEnum 일 때 허용 값
}

// backendKeys 는 PgBouncer 가 [databases] 항목에서 받는 키와 타입.
var backendKeys = map[string]keySpec{
	"host":                      {kind: kindString},
	"port":                      {kind: kindPort},
	"dbname":                    {kind: kindString},
	"user":                      {kind: kindString},
	"password":                  {kind: kindString},
	"auth_user":                 {kind: kindString},
	"auth_query":                {kind: kindString},
	"auth_dbname":               {kind: kindString},
	"pool_size":                 {kind: kindUint},
	"min_pool_size":             {kind: kindUint},
	"reserve_pool_size":         {kind: kindUint},
	"max_db_connections":        {kind: kindUint},
	"max_db_client_connections": {kind: kindUint},
	"server_lifetime":           {kind: kindUint},
	"pool_mode":                 {kind: kindEnum, values: []string{"session", "transaction", "statement"}},
	"load_balance_hosts":        {kind: kindEnum, values: []string{"disable", "round-robin"}},
	"connect_query":             {kind: kindString},
	"client_encoding":           {kind: kindString},
	"datestyle":                 {kind: kindString},
	"timezone":                  {kind: kindString},
	"application_name":          {kind: kindString},
}

// ParseBackend 는 JSON 객체를 읽어 키별 타입을 검증한다. host 는 필수.
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
		spec, ok := backendKeys[k]
		if !ok {
			return nil, fmt.Errorf("unknown key %q", k)
		}
		val, err := spec.parse(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		b[k] = val
	}
	if _, ok := b["host"]; !ok {
		return nil, fmt.Errorf("host is required")
	}
	return b, nil
}

func (s keySpec) parse(v any) (string, error) {
	switch s.kind {
	case kindUint, kindPort:
		num, ok := v.(json.Number)
		if !ok {
			return "", fmt.Errorf("must be a number")
		}
		n, err := strconv.ParseInt(num.String(), 10, 32)
		if err != nil {
			return "", fmt.Errorf("must be an integer")
		}
		if s.kind == kindPort && (n < 1 || n > 65535) {
			return "", fmt.Errorf("must be between 1 and 65535")
		}
		if n < 0 {
			return "", fmt.Errorf("must not be negative")
		}
		return strconv.FormatInt(n, 10), nil
	}

	str, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("must be a string")
	}
	if str == "" {
		return "", fmt.Errorf("must not be empty")
	}
	// pgbouncer.ini 한 줄에 들어가야 하므로 줄바꿈은 받지 않는다.
	if strings.ContainsAny(str, "\r\n\x00") {
		return "", fmt.Errorf("must be a single line")
	}
	if s.kind == kindEnum && !slices.Contains(s.values, str) {
		return "", fmt.Errorf("must be one of %s", strings.Join(s.values, ", "))
	}
	return str, nil
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
