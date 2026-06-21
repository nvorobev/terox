package db

import (
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
)

// Column — типизированное описание колонки результата (Feature 3). Несёт всё, что
// даёт pgx.FieldDescription (а не только имя), чтобы:
//   - сохранять повторяющиеся имена колонок (Occurrence различает SELECT id, id);
//   - детектить дрейф типов между шардами (DataTypeOID + TypeModifier);
//   - корректно форматировать массивы/jsonb/interval/inet/ranges (по OID);
//   - добавлять provenance без конфликтов (Synthetic помечает искусственную колонку);
//   - давать типизированный JSON-экспорт (TypeName — человекочитаемое имя типа).
type Column struct {
	Name           string `json:"name"`
	DataTypeOID    uint32 `json:"type_oid"`
	TypeModifier   int32  `json:"typmod"`
	TypeName       string `json:"type_name"`
	Format         int16  `json:"format"`
	SourceTableOID uint32 `json:"source_table_oid,omitempty"`
	SourceAttr     int16  `json:"source_attr,omitempty"`
	Occurrence     int    `json:"occurrence"`
	Synthetic      bool   `json:"synthetic,omitempty"`
}

// Well-known OID, у которых type modifier несёт человекочитаемую деталь
// (длину/точность). Литералы, а не pgtype-константы, — чтобы не зависеть от того,
// какие именно константы экспортирует конкретная версия pgx.
const (
	oidBPChar      = 1042
	oidVarchar     = 1043
	oidNumeric     = 1700
	oidTime        = 1083
	oidTimestamp   = 1114
	oidTimestamptz = 1184
	oidTimetz      = 1266
	oidInterval    = 1186
	oidBit         = 1560
	oidVarbit      = 1562
)

var (
	typeMapOnce sync.Once
	typeMap     *pgtype.Map
)

func typeNameMap() *pgtype.Map {
	typeMapOnce.Do(func() { typeMap = pgtype.NewMap() })
	return typeMap
}

// TypeName разрешает OID (+ type modifier) в человекочитаемое имя типа, например
// "int4", "varchar(10)", "numeric(10,2)", "timestamptz(3)". Встроенные типы
// разрешаются через дефолтную карту типов pgx БЕЗ обращения к БД; неизвестный OID
// (пользовательский/расширение) деградирует до "oid:NNNN". oid==0 (literal NULL) →
// "unknown".
func TypeName(oid uint32, typmod int32) string {
	return baseTypeName(oid) + typmodSuffix(oid, typmod)
}

func baseTypeName(oid uint32) string {
	if oid == 0 {
		return "unknown"
	}
	if t, ok := typeNameMap().TypeForOID(oid); ok && t.Name != "" {
		return t.Name
	}
	return fmt.Sprintf("oid:%d", oid)
}

// typmodSuffix форматирует деталь type modifier для типов, где она значима
// (varchar(n), numeric(p,s), timestamptz(n) и т.п.). Для остальных — "".
func typmodSuffix(oid uint32, typmod int32) string {
	if typmod < 0 {
		return ""
	}
	switch oid {
	case oidVarchar, oidBPChar:
		if typmod >= 4 {
			return fmt.Sprintf("(%d)", typmod-4)
		}
	case oidNumeric:
		if typmod >= 4 {
			t := typmod - 4
			prec := (t >> 16) & 0xFFFF
			scale := t & 0xFFFF
			if scale == 0 {
				return fmt.Sprintf("(%d)", prec)
			}
			return fmt.Sprintf("(%d,%d)", prec, scale)
		}
	case oidTime, oidTimestamp, oidTimestamptz, oidTimetz, oidInterval, oidBit, oidVarbit:
		return fmt.Sprintf("(%d)", typmod)
	}
	return ""
}
