// Package parser contains the command parser.
//
//nolint:govet
package parser

import (
	"strconv"
	"strings"

	"github.com/squareup/pranadb/command/parser/selector"
	"github.com/squareup/pranadb/errors"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"

	"github.com/squareup/pranadb/common"
)

// DefaultFSP is the default fractional seconds precision for a TIMESTAMP field.
const DefaultFSP = 0

// RawQuery represents raw SQL that can be passed through directly.
type RawQuery struct {
	Tokens []lexer.Token
	Query  []string `(!";")+`
}

func (r *RawQuery) String() string {
	out := strings.Builder{}
	for _, token := range r.Tokens {
		v := token.Value
		if token.Type == parser.Lexer().Symbols()["String"] {
			// THIS IS A HACK! Need to fix participle bug that's stripping the quotes from the raw tokens
			v = strconv.Quote(v)
		}
		out.WriteString(v)
	}
	return out.String()
}

// A Ref to a view, table, column, etc.
type Ref struct {
	Path []string `@Ident ("." @Ident)*`
}

func (r *Ref) String() string {
	return strings.Join(r.Path, ".")
}

// CreateMaterializedView statement.
type CreateMaterializedView struct {
	Name              *Ref                                 `@@`
	OriginInformation []*MaterializedViewOriginInformation `("WITH" "(" @@ ("," @@)* ")")?`
	Query             *RawQuery                            `"AS" @@`
}

type MaterializedViewOriginInformation struct {
	InitialState string `"InitialState" "=" @String`
}

type ColumnDef struct {
	Pos lexer.Position

	Name string `@Ident`

	Type       common.Type `@(("VARCHAR"|"TINYINT"|"INT"|"BIGINT"|"TIMESTAMP"|"DOUBLE"|"DECIMAL"))` // Conversion done by common.Type.Capture()
	Parameters []int       `("(" @Number ("," @Number)* ")")?`                                      // Optional parameters to the type(x [, x, ...])
}

func (c *ColumnDef) ToColumnType() (common.ColumnType, error) {
	ct, ok := common.ColumnTypesByType[c.Type]
	if ok {
		if len(c.Parameters) != 0 {
			return common.ColumnType{}, errors.WithStack(participle.Errorf(c.Pos, ""))
		}
		return ct, nil
	}
	switch c.Type {
	case common.TypeDecimal:
		if len(c.Parameters) != 2 {
			return common.ColumnType{}, participle.Errorf(c.Pos, "Expected DECIMAL(precision, scale)")
		}
		prec := c.Parameters[0]
		scale := c.Parameters[1]
		if prec > 65 || prec < 1 {
			return common.ColumnType{}, participle.Errorf(c.Pos, "Decimal precision must be > 0 and <= 65")
		}
		if scale > 30 || scale < 0 {
			return common.ColumnType{}, participle.Errorf(c.Pos, "decimal scale must be >= 0 and <= 30")
		}
		if scale > prec {
			return common.ColumnType{}, participle.Errorf(c.Pos, "Decimal scale must be <= precision")
		}
		return common.NewDecimalColumnType(c.Parameters[0], c.Parameters[1]), nil
	case common.TypeTimestamp:
		var fsp int8 = DefaultFSP
		if len(c.Parameters) == 1 {
			fsp = int8(c.Parameters[0])
			if fsp < 0 || fsp > 6 {
				return common.ColumnType{}, participle.Errorf(c.Pos, "Timestamp fsp must be >= 0 and <= 6")
			}
		}
		return common.NewTimestampColumnType(fsp), nil
	default:
		panic(c.Type) // If this happens there's something wrong with the parser and/or validation.
	}
}

type TableOption struct {
	PrimaryKey []string   `  "PRIMARY" "KEY" "(" @Ident ( "," @Ident )* ")"`
	Column     *ColumnDef `| @@`
}

type CreateSource struct {
	Name              string                     `@Ident`
	Options           []*TableOption             `"(" @@ ("," @@)* ")"` // Table options.
	OriginInformation []*SourceOriginInformation `"WITH" "(" @@ ("," @@)* ")"`
}

type SourceOriginInformation struct {
	BrokerName     string                        `"BrokerName" "=" @String`
	TopicName      string                        `|"TopicName" "=" @String`
	HeaderEncoding string                        `|"HeaderEncoding" "=" @String`
	KeyEncoding    string                        `|"KeyEncoding" "=" @String`
	ValueEncoding  string                        `|"ValueEncoding" "=" @String`
	IngestFilter   string                        `|"IngestFilter" "=" @String`
	InitialState   string                        `|"InitialState" "=" @String`
	ColSelectors   []*selector.ColumnSelectorAST `|"ColumnSelectors" "=" "(" (@@ ("," @@)*)? ")"`
	Properties     []*TopicInfoProperty          `|"Properties" "=" "(" (@@ ("," @@)*)? ")"`
}

type ColSelector struct {
	Selector string `@String`
}

type TopicInfoProperty struct {
	Key   string `@String "="`
	Value string `@String`
}

type CreateIndex struct {
	Name        string        `@Ident "ON"`
	TableName   string        `@Ident`
	ColumnNames []*ColumnName `"(" @@ ("," @@)* ")"`
}

type ColumnName struct {
	Name string `@Ident`
}

// Create statement.
type Create struct {
	MaterializedView *CreateMaterializedView `  "MATERIALIZED" "VIEW" @@`
	Source           *CreateSource           `| "SOURCE" @@`
	Index            *CreateIndex            `| "INDEX" @@`
}

// Drop statement
type Drop struct {
	MaterializedView bool   `(   @"MATERIALIZED" "VIEW"`
	Source           bool   `  | @"SOURCE"`
	Index            bool   `  | @"INDEX" )`
	Name             string `@Ident `
	TableName        string `("ON" @Ident)?`
}

// Show statement
type Show struct {
	Tables    bool   `(  @"TABLES"`
	Schemas   bool   `| @"SCHEMAS"`
	Indexes   bool   `| @"INDEXES" )`
	TableName string `("ON" @Ident)?`
}

type ConsumerRate struct {
	SourceName string `@Ident`
	Rate       int64  `@Number`
}

// AST root.
type AST struct {
	Select       string        // Unaltered SELECT statement, if any.
	Use          string        `(  "USE" @Ident`
	Drop         *Drop         ` | "DROP" @@ `
	Create       *Create       ` | "CREATE" @@ `
	Show         *Show         ` | "SHOW" @@ `
	Describe     string        ` | "DESCRIBE" @Ident `
	ConsumerRate *ConsumerRate ` | "CONSUMER" "RATE" @@ `
	ResetDdl     string        ` | "RESET" "DDL" @Ident ) ';'?`
}
