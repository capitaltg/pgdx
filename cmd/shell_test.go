package cmd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/chzyer/readline"
)

// childNames returns the (space-trimmed) names of a completer node's immediate children.
func childNames(p readline.PrefixCompleterInterface) map[string]readline.PrefixCompleterInterface {
	out := map[string]readline.PrefixCompleterInterface{}
	for _, c := range p.GetChildren() {
		out[strings.TrimSpace(string(c.GetName()))] = c
	}
	return out
}

func TestHandleLine(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantQuit bool
		wantErr  string // substring expected on the errOut stream ("" = expect nothing)
	}{
		{"exit quits", "exit", true, ""},
		{"quit quits", "quit", true, ""},
		{`\q quits`, `\q`, true, ""},
		{"blank line is a no-op", "    ", false, ""},
		{"comment is a no-op", "# just a note", false, ""},
		{"help lists built-ins", "help", false, "Shell built-ins"},
		{"use without a database shows usage", "use", false, "usage: use <database>"},
		{"use schema without a name shows usage", "use schema", false, "usage: use schema <name>"},
		{"unterminated quote is reported", `query "oops`, false, "unterminated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut strings.Builder
			quit := handleLine(&out, &errOut, nil, tc.line)
			if quit != tc.wantQuit {
				t.Errorf("handleLine(%q) quit = %v, want %v", tc.line, quit, tc.wantQuit)
			}
			if out.Len() != 0 {
				t.Errorf("handleLine(%q) wrote to stdout: %q (built-ins should not)", tc.line, out.String())
			}
			switch {
			case tc.wantErr == "" && errOut.Len() != 0:
				t.Errorf("handleLine(%q) errOut = %q, want empty", tc.line, errOut.String())
			case tc.wantErr != "" && !strings.Contains(errOut.String(), tc.wantErr):
				t.Errorf("handleLine(%q) errOut = %q, want substring %q", tc.line, errOut.String(), tc.wantErr)
			}
		})
	}
}

func TestCompleterCommandTree(t *testing.T) {
	root, ok := newCompleter().(*readline.PrefixCompleter)
	if !ok {
		t.Fatalf("newCompleter() is %T, want *readline.PrefixCompleter", newCompleter())
	}
	top := childNames(root)

	// Tier 1: the verb grammar mirrors the cobra tree.
	for _, want := range []string{"get", "describe", "explain", "query", "status", "shell"} {
		if _, ok := top[want]; !ok {
			t.Errorf("top-level completion missing %q", want)
		}
	}
	// cobra's generated helpers are excluded as noise.
	for _, omit := range []string{"help", "completion"} {
		if _, ok := top[omit]; ok {
			t.Errorf("top-level completion should not include %q", omit)
		}
	}

	// vacuum and analyze offer dynamic table-name completion (Tier 2) too.
	for _, name := range []string{"vacuum", "analyze"} {
		node, ok := top[name]
		if !ok {
			t.Errorf("top-level completion missing %q", name)
			continue
		}
		hasDynamic := false
		for _, c := range node.GetChildren() {
			if pc, ok := c.(*readline.PrefixCompleter); ok && pc.IsDynamic() {
				hasDynamic = true
			}
		}
		if !hasDynamic {
			t.Errorf("%s completion should have a dynamic table-name completer", name)
		}
	}

	// The `use` shell built-in is present with a dynamic database-name child.
	useNode, ok := top["use"]
	if !ok {
		t.Errorf("top-level completion missing the `use` built-in")
	} else {
		hasDynamic := false
		for _, c := range useNode.GetChildren() {
			if pc, ok := c.(*readline.PrefixCompleter); ok && pc.IsDynamic() {
				hasDynamic = true
			}
		}
		if !hasDynamic {
			t.Errorf("`use` completion should have a dynamic database-name completer")
		}
	}

	// get's subcommands are present.
	getSubs := childNames(top["get"])
	for _, want := range []string{"tables", "indexes", "schemas"} {
		if _, ok := getSubs[want]; !ok {
			t.Errorf("get completion missing subcommand %q", want)
		}
	}

	// describe table/view/index each carry a dynamic object-name child (Tier 2) plus the
	// global flags.
	descSubs := childNames(top["describe"])
	for _, name := range []string{"table", "view", "index"} {
		sub, ok := descSubs[name]
		if !ok {
			t.Errorf("describe completion missing %q", name)
			continue
		}
		hasDynamic := false
		for _, c := range sub.GetChildren() {
			if pc, ok := c.(*readline.PrefixCompleter); ok && pc.IsDynamic() {
				hasDynamic = true
			}
		}
		if !hasDynamic {
			t.Errorf("describe %s should have a dynamic object-name completer", name)
		}
		if _, ok := childNames(sub)["--output"]; !ok {
			t.Errorf("describe %s completion missing global flag --output", name)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string
	}{
		{"plain words", "get tables", []string{"get", "tables"}},
		{"collapses whitespace", "  get   tables\t-o json ", []string{"get", "tables", "-o", "json"}},
		{"double quotes group", `query "select 1"`, []string{"query", "select 1"}},
		{"single quotes group", `query 'select 1'`, []string{"query", "select 1"}},
		{
			"single quotes inside double quotes are literal (SQL string literal)",
			`query "select 'a' as x"`,
			[]string{"query", "select 'a' as x"},
		},
		{"backslash escape outside quotes", `query select\ 1`, []string{"query", "select 1"}},
		{"backslash escape inside double quotes", `query "say \"hi\""`, []string{"query", `say "hi"`}},
		{"empty quoted string is an argument", `query ""`, []string{"query", ""}},
		{"adjacent quotes concatenate", `a"b"'c'`, []string{"abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitArgs(tc.line)
			if err != nil {
				t.Fatalf("splitArgs(%q) errored: %v", tc.line, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("splitArgs(%q) = %#v, want %#v", tc.line, got, tc.want)
			}
		})
	}
}

func TestSplitArgsUnterminatedQuote(t *testing.T) {
	for _, line := range []string{`query "select 1`, `query 'select 1`} {
		if _, err := splitArgs(line); err == nil {
			t.Fatalf("splitArgs(%q) = nil error, want unterminated-quote error", line)
		}
	}
}
