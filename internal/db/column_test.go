package db

import "testing"

func TestTypeNameBuiltins(t *testing.T) {
	cases := []struct {
		oid  uint32
		mod  int32
		want string
	}{
		{23, -1, "int4"},
		{20, -1, "int8"},
		{25, -1, "text"},
		{16, -1, "bool"},
		{2950, -1, "uuid"},
		{3802, -1, "jsonb"},
		{1043, 14, "varchar(10)"},       // typmod 14 → len 10
		{1042, 24, "bpchar(20)"},        // char(20)
		{1700, 655366, "numeric(10,2)"}, // ((10<<16)|2)+4
		{1700, 524292, "numeric(8)"},    // ((8<<16)|0)+4 → scale 0 omitted
		{1184, 3, "timestamptz(3)"},
		{0, -1, "unknown"}, // literal NULL
	}
	for _, c := range cases {
		if got := TypeName(c.oid, c.mod); got != c.want {
			t.Errorf("TypeName(%d,%d) = %q, want %q", c.oid, c.mod, got, c.want)
		}
	}
}

func TestTypeNameUnknownOID(t *testing.T) {
	// Пользовательский/расширенческий OID, которого нет в дефолтной карте.
	got := TypeName(999999, -1)
	if got != "oid:999999" {
		t.Errorf("unknown OID should degrade to oid:999999, got %q", got)
	}
}
