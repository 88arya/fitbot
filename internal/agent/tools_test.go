package agent

import "testing"

func TestCanonical(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{ "timezone": null, "name": "Sam" }`, `{"name":"Sam","timezone":null}`},
		{`{"id": 9007199254740993}`, `{"id":9007199254740993}`},
		{`{"ml": 500.0, "n": {"b": 1, "a": 2}}`, `{"ml":500.0,"n":{"a":2,"b":1}}`},
	} {
		got, err := canonical(c.in)
		if err != nil || string(got) != c.want {
			t.Errorf("canonical(%s) = %s, %v; want %s", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{``, `{"a":`, `{"a":1} {"b":2}`} {
		if _, err := canonical(in); err == nil {
			t.Errorf("canonical(%q) accepted invalid JSON", in)
		}
	}
}
