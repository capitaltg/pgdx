package cmd

import "testing"

func TestExplicitConnTarget(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		name string
		dsn  string
		vars map[string]string
		want bool
	}{
		{"nothing set", "", nil, false},
		{"dsn wins", "postgres://h/db", nil, true},
		{"PGSERVICE set", "", map[string]string{"PGSERVICE": "prod"}, true},
		{"PGHOST set (the bug)", "", map[string]string{"PGHOST": "localhost"}, true},
		{"PGHOSTADDR set", "", map[string]string{"PGHOSTADDR": "127.0.0.1"}, true},
		{"only PGUSER set is not a target", "", map[string]string{"PGUSER": "alice"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := explicitConnTarget(c.dsn, env(c.vars)); got != c.want {
				t.Fatalf("explicitConnTarget(%q, %v) = %v, want %v", c.dsn, c.vars, got, c.want)
			}
		})
	}
}
