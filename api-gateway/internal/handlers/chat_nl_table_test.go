package handlers

import (
	"reflect"
	"strings"
	"testing"

	"api-gateway/internal/chat"
)

// TestParseTableIntent locks KI-NLCHAT-TABLENAME-IGNORED and
// KI-NLCHAT-TABLE-LIST-IGNORED: table names the request spells out, one or a
// list, must be extracted so the create can pre-select them and skip the
// table-selection HITL. Phrasings that name no table ("all tables", "the
// database", "everything") return nil so they still reach the HITL.
func TestParseTableIntent(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want []string
	}{
		// Positive — singular "<name> table"
		{"copy the X table", "copy the customers table from test-mysql into pg-dest", []string{"customers"}},
		{"sync the X table", "sync the orders table from mysql to postgres", []string{"orders"}},
		{"bare X table", "move customers table to pg-dest", []string{"customers"}},
		{"underscore name", "copy the order_items table", []string{"order_items"}},
		{"schema-qualified", "sync the public.customers table", []string{"public.customers"}},
		// Positive — "table <name>"
		{"table then name", "sync from mysql, table customers, to postgres", []string{"customers"}},
		// Negative — no table named → defer to HITL
		{"no table keyword", "sync data from mysql to postgres", nil},
		{"everything", "copy everything from test-mysql to pg-dest", nil},
		{"bare the table", "sync the table from mysql to postgres", nil},
		// Negative — plural with no names → defer to HITL
		{"all tables", "sync all tables from mysql to postgres", nil},
		{"every table", "replicate every table from mysql", nil},
		{"engine tables, one name", "copy mysql tables to pg-dest", nil},
		// Positive — an explicit LIST of names (KI-NLCHAT-TABLE-LIST-IGNORED). The
		// user named every table; each one is checked against the schema cache
		// before anything is pre-selected, all-or-nothing.
		{"plural tables", "copy the users and orders tables to pg-dest", []string{"users", "orders"}},
		{"verb list from", "move orders and customers from pg to bq", []string{"orders", "customers"}},
		{"verb list tables from", "move orders and customers tables from pg to bq", []string{"orders", "customers"}},
		{"comma list with and", "copy orders, customers and payments from mysql to postgres", []string{"orders", "customers", "payments"}},
		{"oxford comma", "sync orders, customers, and payments from mysql", []string{"orders", "customers", "payments"}},
		{"ampersand", "sync orders & customers from mysql to postgres", []string{"orders", "customers"}},
		{"tables keyword first", "sync tables orders, customers from mysql to postgres", []string{"orders", "customers"}},
		{"tables colon", "sync mysql to postgres, tables: orders, customers", []string{"orders", "customers"}},
		{"quoted names", "move \"orders\" and `customers` from pg to bq", []string{"orders", "customers"}},
		{"qualified names", "copy public.orders and sales.customers from pg to bq", []string{"public.orders", "sales.customers"}},
		{"only one", "move only orders from pg to bq", []string{"orders"}},
		{"single verb object", "move orders from pg to bq", []string{"orders"}},
		{"mixed case", "Move Orders and Customers from PG to BQ", []string{"orders", "customers"}},
		// Positive — a document store calls a table a collection. "Copy the matches
		// collection from my MongoDB Source" pre-selected nothing on prod
		// (2026-09-30), because only "table(s)" was read as the noun.
		{"the X collection", "copy the matches collection from my mongodb source to my gcs dest", []string{"matches"}},
		{"collection then name", "sync from mongo, collection customers, to postgres", []string{"customers"}},
		{"plural collections", "sync the users and orders collections to bq", []string{"users", "orders"}},
		{"verb list collections from", "move orders and customers collections from mongo to bq", []string{"orders", "customers"}},
		{"collections colon", "sync mongo to gcs, collections: orders, customers", []string{"orders", "customers"}},
		// Negative — the noun alone names nothing
		{"bare the collection", "sync the collection from mongo to postgres", nil},
		{"all collections", "sync all collections from mongo to gcs", nil},
		{"engine collections, one name", "copy mongodb collections to gcs", nil},
		// Negative — generic objects of the verb are not names
		{"all data", "copy all data from mysql to postgres", nil},
		{"data", "move data from pg to bq", nil},
		{"them", "copy them from mysql to postgres", nil},
		{"the database", "copy the database from mysql to postgres", nil},
		{"whole database", "copy the whole database from mysql to postgres", nil},
		{"both", "sync both from mysql", nil},
		{"no from", "sync pg-source to bq-dest", nil},
		{"a stop word in the list", "move orders and everything from pg to bq", nil},
		// Negative — a sync MODE between the verb and "from" is not a table. The
		// gateway stores a CDC request as "Stream CDC from <src> to <dst>. <request>",
		// and the pause card read `cdc` as a named table (prod a6ca2103, 2026-10-01).
		{"gateway CDC prefix", "Stream CDC from postgresql to mongodb. Stream changes from Postgresql Source to MongoDB Dest", nil},
		{"mode word cdc", "replicate cdc from pg to mongo", nil},
		{"mode word streaming", "sync streaming from pg to bq", nil},
		{"mode word realtime", "copy realtime from pg to bq", nil},
		{"mode word real-time", "sync real-time from pg to bq", nil},
		{"mode word incrementally", "load incrementally from pg to bq", nil},
		{"mode word continuously", "replicate continuously from pg to bq", nil},
		{"mode word updates", "stream updates from pg to bq", nil},
		{"a mode word beside a name", "stream cdc and orders from pg to bq", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseTableIntent(c.msg)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseTableIntent(%q) = %v, want %v", c.msg, got, c.want)
			}
		})
	}
}

// TestShouldPreselectNamedTables locks the safety gate: pre-select ONLY on a clean
// cache resolution; any missing / ambiguous / cold-cache result defers to HITL.
func TestShouldPreselectNamedTables(t *testing.T) {
	cases := []struct {
		name      string
		qualified []string
		missing   []string
		ambiguous map[string][]string
		ok        bool
		want      bool
	}{
		{"clean resolve", []string{"public.customers"}, nil, nil, true, true},
		{"cache cold (ok=false)", nil, nil, nil, false, false},
		{"missing name", nil, []string{"custmers"}, nil, true, false},
		{"ambiguous name", nil, nil, map[string][]string{"customers": {"a.customers", "b.customers"}}, true, false},
		{"empty qualified", nil, nil, nil, true, false},
		{"partial: one qualified but one missing", []string{"public.orders"}, []string{"custmers"}, nil, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldPreselectNamedTables(c.qualified, c.missing, c.ambiguous, c.ok); got != c.want {
				t.Fatalf("shouldPreselectNamedTables(%v,%v,%v,%v) = %v, want %v",
					c.qualified, c.missing, c.ambiguous, c.ok, got, c.want)
			}
		})
	}
}

// TestQualifyNamedTables locks the name match the pre-select runs on a table list,
// cached or read live: a bare name resolves when exactly one schema holds it, a
// qualified name only on an exact match, and case never matters.
func TestQualifyNamedTables(t *testing.T) {
	known := []schemaTableRef{
		{Schema: "datingapp", Name: "matches"},
		{Schema: "datingapp", Name: "subscriptions"},
		{Schema: "public", Name: "users"},
		{Schema: "archive", Name: "users"},
		{Name: "events"},
	}
	cases := []struct {
		name      string
		in        []string
		qualified []string
		missing   []string
		ambiguous map[string][]string
	}{
		{"bare name in one schema", []string{"matches"}, []string{"datingapp.matches"}, []string{}, map[string][]string{}},
		{"case-insensitive", []string{"Subscriptions"}, []string{"datingapp.subscriptions"}, []string{}, map[string][]string{}},
		{"qualified exact", []string{"DatingApp.Matches"}, []string{"datingapp.matches"}, []string{}, map[string][]string{}},
		{"schemaless table", []string{"events"}, []string{"events"}, []string{}, map[string][]string{}},
		{"missing", []string{"matches", "payments"}, []string{"datingapp.matches"}, []string{"payments"}, map[string][]string{}},
		{"qualified in the wrong schema", []string{"public.matches"}, []string{}, []string{"public.matches"}, map[string][]string{}},
		{"ambiguous", []string{"users"}, []string{}, []string{}, map[string][]string{"users": {"archive.users", "public.users"}}},
		{"named twice", []string{"matches", "datingapp.matches"}, []string{"datingapp.matches"}, []string{}, map[string][]string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, m, a := qualifyNamedTables(known, c.in)
			if !reflect.DeepEqual(q, c.qualified) || !reflect.DeepEqual(m, c.missing) || !reflect.DeepEqual(a, c.ambiguous) {
				t.Fatalf("qualifyNamedTables(%v) = %v, %v, %v; want %v, %v, %v", c.in, q, m, a, c.qualified, c.missing, c.ambiguous)
			}
		})
	}
}

// TestConfirmationNamedTables locks the confirmation union: the tables the intent
// classifier extracted are no longer dropped at confirmation, and a name added on
// the confirmation turn joins the ones from the original request.
func TestConfirmationNamedTables(t *testing.T) {
	cases := []struct {
		name    string
		pending *chat.PendingIntent
		message string
		want    []string
	}{
		{"nil pending", nil, "yes", nil},
		{"classifier tables survive", &chat.PendingIntent{Tables: []string{"Orders", "customers"}, OriginalRequest: "sync pg to bq"}, "yes", []string{"orders", "customers"}},
		{"classifier wildcards dropped", &chat.PendingIntent{Tables: []string{"*", "all", " "}, OriginalRequest: "sync pg to bq"}, "yes", nil},
		{"request parse", &chat.PendingIntent{OriginalRequest: "move orders and customers from pg to bq"}, "yes", []string{"orders", "customers"}},
		{"union, no duplicates", &chat.PendingIntent{Tables: []string{"orders"}, OriginalRequest: "move orders and customers from pg to bq"}, "yes, also table payments", []string{"orders", "customers", "payments"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := confirmationNamedTables(c.pending, c.message); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("confirmationNamedTables = %v, want %v", got, c.want)
			}
		})
	}
}

// TestNamedTablesNote locks what the chat reply says about the tables the user
// named: which were pre-selected, or why the table-selection step still asks.
func TestNamedTablesNote(t *testing.T) {
	cases := []struct {
		name      string
		requested []string
		qualified []string
		missing   []string
		ambiguous map[string][]string
		ok        bool
		wantHas   []string
	}{
		{"none requested", nil, nil, nil, nil, true, nil},
		{"pre-selected", []string{"orders", "customers"}, []string{"public.orders", "public.customers"}, nil, nil, true, []string{"`public.orders`", "`public.customers`", "only"}},
		{"source unreadable", []string{"orders"}, nil, nil, nil, false, []string{"`orders`", "couldn't read", "table-selection step"}},
		{"missing", []string{"orders", "custmers"}, []string{"public.orders"}, []string{"custmers"}, nil, true, []string{"`custmers`", "table-selection step"}},
		{"ambiguous", []string{"customers"}, nil, nil, map[string][]string{"customers": {"a.customers", "b.customers"}}, true, []string{"`customers`", "more than one schema", "table-selection step"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := namedTablesNote(c.requested, c.qualified, c.missing, c.ambiguous, c.ok)
			if len(c.wantHas) == 0 {
				if got != "" {
					t.Fatalf("namedTablesNote = %q, want empty", got)
				}
				return
			}
			for _, w := range c.wantHas {
				if !strings.Contains(got, w) {
					t.Fatalf("namedTablesNote = %q, missing %q", got, w)
				}
			}
		})
	}
}

// TestNamedTablesCardLine: the Confirm card lists the tables the request named,
// so the user sees them before saying yes.
func TestNamedTablesCardLine(t *testing.T) {
	if got := namedTablesCardLine(&chat.PendingIntent{OriginalRequest: "sync pg to bq"}); got != "" {
		t.Fatalf("no tables named: card line = %q, want empty", got)
	}
	got := namedTablesCardLine(&chat.PendingIntent{OriginalRequest: "move orders and customers from pg to bq"})
	for _, w := range []string{"`orders`", "`customers`"} {
		if !strings.Contains(got, w) {
			t.Fatalf("card line = %q, missing %q", got, w)
		}
	}
}
