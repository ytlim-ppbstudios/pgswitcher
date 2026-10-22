package main

import "testing"

func TestParseConnstring(t *testing.T) {
	cases := map[string]string{
		"host=db.example.com":                             "host=db.example.com",
		"  host=db.example.com   port=6543 pool_size=50 ": "host=db.example.com port=6543 pool_size=50",
		"host=db password='a b''c' dbname=app":            "host=db password='a b''c' dbname=app",
		"host=db password='x'":                            "host=db password=x",
	}
	for in, want := range cases {
		cs, err := ParseConnstring(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got := cs.String(); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}

	cs, _ := ParseConnstring("host=db password='a b''c'")
	if got := cs.Get("password"); got != "a b'c" {
		t.Errorf("password = %q", got)
	}
	if got := cs.Redacted(); got != "host=db password=****" {
		t.Errorf("redacted = %q", got)
	}
}

func TestParseConnstringErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"port=5432",
		"host=db\nadmin_users=x",
		"host=db host=other",
		"host=db foo=bar",
		"host=db port=abc",
		"host=db port=70000",
		"host=db password='open",
		"host=db password=''",
		"host=db password='a'b",
		"host=it's",
		"host=",
		"host",
	} {
		if _, err := ParseConnstring(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}
