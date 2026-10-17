package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"

	"github.com/flyingEirc/IotProject/internal/fsutil"
)

type binlogValidation struct {
	source, target, next string
	start, offset        int64
	foundStart, rotated  bool
	sql                  *parser.Parser
}

// MySQL's rewrite-db rewrites table maps and USE, not schema-qualified SQL.
// Inspect through maintained parsers rather than guessing from SQL text.
func validateBinlog(ctx context.Context, path string, start int64, source, target, next string) (retErr error) {
	file, err := fsutil.Open(path)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, file)
	var magic [4]byte
	if _, err = io.ReadFull(file, magic[:]); err != nil || !bytes.Equal(magic[:], []byte{0xfe, 'b', 'i', 'n'}) {
		return errors.New("invalid raw MySQL binlog")
	}
	v := &binlogValidation{source: source, target: target, next: next, start: start, offset: 4, foundStart: start == 4, sql: parser.New()}
	if err = v.readEvents(ctx, file); err != nil {
		return err
	}
	if !v.foundStart {
		return errors.New("snapshot position is not an event boundary in archived binlog")
	}
	if !v.rotated {
		return errors.New("archived binlog lacks its closing rotation boundary")
	}
	return nil
}
func (v *binlogValidation) readEvents(ctx context.Context, file io.Reader) error {
	p := replication.NewBinlogParser()
	p.SetVerifyChecksum(true)
	// We need metadata, not decoded row values. The official client validates/replays rows.
	p.SetRowsEventDecodeFunc(func(_ *replication.RowsEvent, _ []byte) error { return nil })
	var header [replication.EventHeaderSize]byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := readEventHeader(file, header[:])
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if v.offset == v.start {
			v.foundStart = true
		}
		reader := io.MultiReader(bytes.NewReader(header[:]), io.LimitReader(file, int64(h.EventSize)-replication.EventHeaderSize))
		if _, err = p.ParseSingleEvent(reader, v.inspect); err != nil {
			return errors.New("binlog validation failed: unsupported event, unsafe database scope, checksum, or chain boundary")
		}
		v.offset += int64(h.EventSize)
	}
}
func readEventHeader(r io.Reader, header []byte) (h replication.EventHeader, err error) {
	n, err := io.ReadFull(r, header)
	if errors.Is(err, io.EOF) && n == 0 {
		return h, io.EOF
	}
	if err != nil {
		return h, errors.New("truncated binlog event header")
	}
	if h.Decode(header) != nil || h.EventSize < replication.EventHeaderSize || h.EventSize > 64<<20 {
		return h, errors.New("invalid or oversized binlog event (maximum 64 MiB)")
	}
	if h.EventType == replication.TRANSACTION_PAYLOAD_EVENT {
		return h, errors.New("compressed binlog transactions require a separately validated decoder; disable binlog_transaction_compression and create a new baseline")
	}
	return h, nil
}
func (v *binlogValidation) inspect(event *replication.BinlogEvent) error {
	if rotate, ok := event.Event.(*replication.RotateEvent); ok {
		if string(rotate.NextLogName) != v.next || rotate.Position != 4 {
			return errors.New("binlog rotation does not match declared chain boundary")
		}
		v.rotated = true
	}
	if v.offset < v.start {
		return nil
	}
	switch ev := event.Event.(type) {
	case *replication.TableMapEvent:
		if v.source != v.target && string(ev.Schema) == v.target {
			return errors.New("renamed destination also occurs in source logs; rewrite-db would mix databases")
		}
	case *replication.QueryEvent:
		return validateQuery(v.sql, string(ev.Query), string(ev.Schema), v.source, v.target)
	}
	return nil
}
func validateQuery(p *parser.Parser, query, schema, source, target string) error {
	if source != target && schema == target {
		return errors.New("renamed destination also occurs in source logs")
	}
	stmts, _, err := p.ParseSQL(query)
	if err != nil {
		return errors.New("cannot safely inspect binlog SQL; a new baseline is required")
	}
	for _, stmt := range stmts {
		if err = validateStatement(stmt, schema, source, target); err != nil {
			return err
		}
	}
	return nil
}
func validateStatement(stmt ast.StmtNode, schema, source, target string) error {
	switch stmt.(type) {
	case *ast.BeginStmt, *ast.CommitStmt, *ast.RollbackStmt:
		return nil
	}
	visitor := &schemaScope{source: source, renamed: source != target}
	_, valid := stmt.Accept(visitor)
	if schema != source {
		// Database filters use the default schema, so a qualified source DDL would be lost.
		if visitor.refers {
			return errors.New("DDL changes the protected database without USE; create a new baseline")
		}
		return nil
	}
	if err := validateStatementKind(stmt, source != target); err != nil {
		return err
	}
	if !valid || visitor.invalid {
		return errors.New("schema-qualified or cross-database DDL cannot be safely redirected")
	}
	return nil
}
func validateStatementKind(stmt ast.StmtNode, renamed bool) error {
	switch stmt.(type) {
	case *ast.CreateDatabaseStmt, *ast.DropDatabaseStmt, *ast.AlterDatabaseStmt:
		return errors.New("database-level DDL requires a new baseline")
	case *ast.ProcedureInfo, *ast.DropProcedureStmt:
		if renamed {
			return errors.New("stored program DDL cannot be safely renamed")
		}
	case *ast.AnalyzeTableStmt:
	default:
		if _, ok := stmt.(ast.DDLNode); !ok {
			return errors.New("statement-based data changes are not supported in a ROW backup chain")
		}
	}
	return nil
}

type schemaScope struct {
	source                   string
	renamed, invalid, refers bool
}

func (v *schemaScope) Enter(n ast.Node) (ast.Node, bool) {
	if table, ok := n.(*ast.TableName); ok && strings.EqualFold(table.Schema.O, v.source) {
		v.refers = true
	}
	if table, ok := n.(*ast.TableName); ok && table.Schema.O != "" && (v.renamed || table.Schema.O != v.source) {
		v.invalid = true
	}
	return n, false
}
func (v *schemaScope) Leave(n ast.Node) (ast.Node, bool) { return n, true }
