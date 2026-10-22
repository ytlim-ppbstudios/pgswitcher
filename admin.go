package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Admin 은 PgBouncer 관리 콘솔(가상 DB "pgbouncer")에 명령을 보낸다.
// 관리 콘솔은 simple query 프로토콜만 받으므로 pgconn 을 직접 쓴다.
type Admin struct {
	connString string
}

func NewAdmin(port int, password string) *Admin {
	return &Admin{connString: fmt.Sprintf(
		"host=127.0.0.1 port=%d dbname=pgbouncer user=%s password='%s' sslmode=disable connect_timeout=3",
		port, adminUser, escapeConnValue(password),
	)}
}

// Rows 는 SHOW 결과 한 개. 값은 모두 텍스트 또는 null.
type Rows []map[string]*string

// Query 는 관리 명령 하나를 실행하고 마지막 결과의 행을 돌려준다.
func (a *Admin) Query(ctx context.Context, sql string) (Rows, error) {
	conn, err := pgconn.Connect(ctx, a.connString)
	if err != nil {
		return nil, fmt.Errorf("connect admin console: %w", err)
	}
	defer conn.Close(context.Background())

	results, err := conn.Exec(ctx, sql).ReadAll()
	if err != nil {
		return nil, err
	}
	rows := Rows{}
	for _, r := range results {
		if r.Err != nil {
			return nil, r.Err
		}
		rows = rows[:0]
		for _, values := range r.Rows {
			row := make(map[string]*string, len(values))
			for i, fd := range r.FieldDescriptions {
				if values[i] == nil {
					row[fd.Name] = nil
					continue
				}
				v := string(values[i])
				row[fd.Name] = &v
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func escapeConnValue(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' || s[i] == '\\' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}
