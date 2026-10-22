package main

import "testing"

func TestParseBackend(t *testing.T) {
	cases := map[string]string{
		`{"host": "db.example.com"}`:                                     "host=db.example.com",
		`{"pool_size": 50, "port": 6543, "host": "db"}`:                  "host=db port=6543 pool_size=50",
		`{"host": "db", "password": "a b'c", "application_name": "pgb"}`: "host=db application_name=pgb password='a b''c'",
	}
	for in, want := range cases {
		b, err := ParseBackend([]byte(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := b.String(); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}

	b, _ := ParseBackend([]byte(`{"host": "db", "password": "secret"}`))
	if got := b.Redacted()["password"]; got != "****" {
		t.Errorf("redacted password = %q", got)
	}
	if got := b["password"]; got != "secret" {
		t.Errorf("Redacted changed the original: %q", got)
	}
}

func TestParseBackendErrors(t *testing.T) {
	for _, in := range []string{
		``,
		`host=db port=5432`,
		`[]`,
		`{"port": 5432}`,
		`{"host": ""}`,
		`{"host": "db", "foo": "bar"}`,
		`{"host": "db\nadmin_users = x"}`,
		`{"host": "db", "port": "abc"}`,
		`{"host": "db", "port": 70000}`,
		`{"host": "db", "port": true}`,
		`{"host": {"a": 1}}`,
		`{"host": "db"} {"host": "other"}`,
	} {
		if _, err := ParseBackend([]byte(in)); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}
