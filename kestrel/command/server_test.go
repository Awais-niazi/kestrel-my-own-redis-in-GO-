package command

import (
	"strings"
	"testing"

	"kestrel/resp"
)

// TestCommandDocsNamesSubcommandsInFull is a regression test for a bug that
// crashed redis-cli.
//
// Subcommands must be reported as "parent|child". redis-cli builds its hint
// table from these names and splits each on the separator to recover the
// parent; given a bare "get" it has nothing to split and segfaults. The bare
// name is ambiguous in any case -- several containers have a GET -- so it
// does not identify the command it describes.
//
// Found by piping commands to a real redis-cli rather than by any test here,
// which is the argument for doing that at all.
func TestCommandDocsNamesSubcommandsInFull(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	reply := s.do("COMMAND", "DOCS", "CONFIG")
	if len(reply.Elems) != 2 {
		t.Fatalf("COMMAND DOCS CONFIG returned %d elements", len(reply.Elems))
	}
	doc := reply.Elems[1]

	var subs resp.Value
	for i := 0; i+1 < len(doc.Elems); i += 2 {
		if string(doc.Elems[i].Str) == "subcommands" {
			subs = doc.Elems[i+1]
		}
	}
	if len(subs.Elems) == 0 {
		t.Fatalf("CONFIG reports no subcommands:\n%s", str(doc))
	}
	for i := 0; i+1 < len(subs.Elems); i += 2 {
		name := string(subs.Elems[i].Str)
		if !strings.HasPrefix(name, "config|") {
			t.Errorf("subcommand is named %q, want a \"config|\" prefix", name)
		}
	}
}

// TestEverySubcommandIsNamedInFull applies the same rule to every container,
// so a new one cannot reintroduce the crash.
func TestEverySubcommandIsNamedInFull(t *testing.T) {
	h := newTestHost(t)
	s := newSession(t, h)

	table, err := NewTable(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range table.all {
		if len(d.Subcommands) == 0 {
			continue
		}
		reply := s.do("COMMAND", "DOCS", d.Name)
		if len(reply.Elems) < 2 {
			continue
		}
		doc := reply.Elems[1]
		for i := 0; i+1 < len(doc.Elems); i += 2 {
			if string(doc.Elems[i].Str) != "subcommands" {
				continue
			}
			subs := doc.Elems[i+1]
			for j := 0; j+1 < len(subs.Elems); j += 2 {
				name := string(subs.Elems[j].Str)
				want := strings.ToLower(d.Name) + "|"
				if !strings.HasPrefix(name, want) {
					t.Errorf("%s reports a subcommand named %q, want a %q prefix",
						d.Name, name, want)
				}
			}
		}
	}
}
