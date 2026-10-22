# pgswitcher

PgBouncer를 자식 프로세스로 띄우고, 백엔드 전환과 설정 변경을 HTTP API로 하는 경량 서버.
DB 이전 때 앱들은 PgBouncer에만 붙여 두고, 백엔드만 바꿔 DNS 전파를 기다리지 않고 한 곳에서 전환하려고 만들었다.

- 이미지는 `edoburu/pgbouncer:v1.26.1-p0` 위에 pgswitcher 바이너리만 얹는다. PgBouncer 코드는 이 레포에 없다.
- 모든 DB 이름을 백엔드 연결 문자열 하나로 보낸다 (`[databases] * = <PGSW_BACKEND>`).
- 시작할 때는 환경변수(예: Secrets Manager에서 주입한 값)로 초기 설정을 만들고, 이후 변경은 API → 파일 재작성 → `RELOAD` 순서로 반영한다.
- API로 바꾼 값은 메모리와 컨테이너 안 파일에만 남는다. 재시작하면 환경변수 값으로 돌아가므로, 유지하려면 초기값 쪽도 같이 바꾼다.

## 구조

```
:5432  PgBouncer   앱 접속
:8080  pgswitcher  HTTP API (관리 콘솔에 127.0.0.1:5432로 붙어 명령 실행)

/etc/pgbouncer/pgbouncer.ini   공통 설정, 마지막 줄에서 databases.ini를 %include
/etc/pgbouncer/databases.ini   백엔드 연결 문자열. 전환하면 이 파일만 바뀐다
/etc/pgbouncer/userlist.txt    PGSW_USERLIST + 관리 계정 pgbouncer_admin
```

pgswitcher가 PID 1이다. SIGTERM, SIGINT, SIGHUP은 PgBouncer에 넘기고, PgBouncer가 종료되면 같은 종료 코드로 끝난다.

## 환경변수

| 이름 | 필수 | 설명 |
|---|---|---|
| `PGSW_BACKEND` | O | 초기 백엔드 연결 문자열 (아래 형식). 예: `host=old.rds.amazonaws.com port=5432` |
| `PGSW_TOKEN` | O | 변경 API의 Bearer 토큰 |
| `PGSW_ADMIN_PASSWORD` | O | 관리 콘솔 계정 `pgbouncer_admin` 비밀번호 |
| `PGSW_USERLIST` | | userlist.txt 본문. 한 줄에 `"user" "plain-password"`. 평문이어야 앱 쪽 SCRAM과 백엔드 로그인을 둘 다 한다 |
| `PGSW_TLS_KEY`, `PGSW_TLS_CERT` | | 앱 → PgBouncer TLS 키/인증서 (PEM). 넣으면 `client_tls_sslmode = prefer` |
| `PGBOUNCER_<설정>` | | PgBouncer 설정 덮어쓰기. 예: `PGBOUNCER_POOL_MODE=transaction`. 아래 변경 가능 목록만 허용 |
| `PGSW_LISTEN` | | API 주소 (기본 `:8080`) |
| `PGSW_PGBOUNCER_PORT` | | PgBouncer 포트 (기본 `5432`) |
| `PGSW_CONFIG_DIR` | | 설정 파일 위치 (기본 `/etc/pgbouncer`) |

기본 설정: `pool_mode=session`, `max_client_conn=5000`, `default_pool_size=1000`, `server_tls_sslmode=require`, `query_wait_timeout=120` 등 (`config.go`의 `defaultSettings`).

인증은 `scram-sha-256` 고정이다.

## 백엔드 연결 문자열

PgBouncer `[databases]` 항목의 값 형식 그대로다. 공백으로 구분한 `key=value`, 공백이나 따옴표가 든 값은 작은따옴표로 감싸고 따옴표는 두 번 쓴다.

```
host=db.example.com port=5432
host=db.example.com port=5432 pool_size=50 application_name=pgbouncer
```

- `host`는 필수. 키는 PgBouncer가 받는 것만 허용 (`dbname`, `host`, `port`, `user`, `password`, `pool_size`, `pool_mode`, `max_db_connections` 등. `connstring.go` 참고)
- 와일드카드(`*`) 항목이라 `dbname`을 넣으면 모든 DB 이름이 그 DB로 간다. 보통은 넣지 않는다
- 인증은 userlist로 하므로 `user`/`password`도 보통 넣지 않는다. 넣은 경우 API 응답과 로그에서 password는 가린다

## API

조회(GET)는 토큰 없이, 변경은 `Authorization: Bearer $PGSW_TOKEN`이 필요하다.

| 메서드 | 경로 | 설명 |
|---|---|---|
| GET | `/healthz` | PgBouncer 프로세스와 관리 콘솔 응답만 확인 (`SHOW VERSION`). 백엔드는 보지 않는다 |
| GET | `/stats` | `SHOW STATE/STATS/POOLS/DATABASES/LISTS`를 JSON으로. 관리 콘솔이 응답 없으면 503이라 헬스체크로도 쓸 수 있다. `?sections=state,pools`로 골라 받기 |
| GET | `/config` | 백엔드 연결 문자열(password는 `****`), 설정 |
| PUT | `/backend` | `{"conn":"host=... port=5432"}` 백엔드 교체 → RELOAD |
| PATCH | `/settings` | `{"default_pool_size":"500"}` 설정 변경 → RELOAD |
| POST | `/pause` | `PAUSE`. `?timeout=10s` 안에 끝나지 않으면 202 `pausing`을 주고 백그라운드에서 계속 기다린다 |
| POST | `/resume` | `RESUME` |
| POST | `/reload` | `RELOAD` |
| POST | `/reconnect` | `RECONNECT` (서버 커넥션을 반납되는 대로 새로 맺음) |

RELOAD가 실패하면 파일을 이전 내용으로 되돌리고 에러를 준다.

`/healthz`가 백엔드를 보지 않는 이유: 전환 중 기존 DB를 `NOLOGIN`으로 막는 구간에 헬스체크가 실패하면 오케스트레이터가 PgBouncer를 재시작해 버린다.

변경 가능한 설정: `pool_mode`, `max_client_conn`, `default_pool_size`, `min_pool_size`, `reserve_pool_size`, `reserve_pool_timeout`, `max_db_connections`, `max_user_connections`, `server_idle_timeout`, `server_lifetime`, `server_connect_timeout`, `server_login_retry`, `server_tls_sslmode`, `query_timeout`, `query_wait_timeout`, `client_idle_timeout`, `idle_transaction_timeout`, `ignore_startup_parameters`, `log_connections`, `log_disconnections`, `log_pooler_errors`, `stats_period`, `verbose`.
리슨 주소, 인증, TLS 파일 경로는 바꿀 수 없다.

## 전환 예시

```sh
A=http://pgbouncer.example.internal:8080
H="Authorization: Bearer $PGSW_TOKEN"

curl -X POST -H "$H" "$A/pause?timeout=1s"     # 대기 상태 진입. 새 요청은 PgBouncer에서 기다린다
# 기존 DB: NOLOGIN + 세션 종료 → PAUSE 완료. CDC 반영 대기
curl "$A/stats?sections=state,pools"           # paused=yes, cl_waiting 확인
curl -X PUT -H "$H" "$A/backend" -d '{"conn":"host=new.rds.amazonaws.com port=5432"}'
curl -X POST -H "$H" "$A/resume"               # 대기하던 요청이 새 백엔드로 붙는다
```

session 모드에서 `PAUSE`는 모든 서버 커넥션이 반납돼야 끝난다. 기존 세션을 끊기 전에는 `pausing` 상태로 남는다.

## 개발

```sh
go test ./...
docker build -t pgswitcher .
docker build --build-arg PGBOUNCER_IMAGE=<복제한 레지스트리>/pgbouncer:v1.26.1-p0 -t pgswitcher .
```
