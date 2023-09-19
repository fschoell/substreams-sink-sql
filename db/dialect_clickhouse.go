package db

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/streamingfast/cli"
	sink "github.com/streamingfast/substreams-sink"
	"go.uber.org/zap"
)

type clickhouseDialect struct{}

// Clickhouse should be used to insert a lot of data in batches. The current official clickhouse
// driver doesn't support Transactions for multiple tables. The only way to add in batches is 
// creating a transaction for a table, adding all rows and commiting it.
func (d clickhouseDialect) Flush(tx *sql.Tx, ctx context.Context, l *Loader, outputModuleHash string, cursor *sink.Cursor) (int, error) {
	var entryCount int
	for entriesPair := l.entries.Oldest(); entriesPair != nil; entriesPair = entriesPair.Next() {
		tableName := entriesPair.Key
		entries := entriesPair.Value
		tx, err := l.DB.BeginTx(ctx, nil)
		if err != nil {
			return entryCount, fmt.Errorf("failed to begin db transaction")
		}

		if l.tracer.Enabled() {
			l.logger.Debug("flushing table entries", zap.String("table_name", tableName), zap.Int("entry_count", entries.Len()))
		}
		info := l.tables[tableName]
		columns := make([]string, 0, len(info.columnsByName))
		for column := range info.columnsByName {
			columns = append(columns, column)
		}
		sort.Strings(columns)
		query := fmt.Sprintf(
			"INSERT INTO %s.%s (%s)",
			EscapeIdentifier(l.schema),
			EscapeIdentifier(tableName),
			strings.Join(columns, ","))
		batch, err := tx.Prepare(query)
		if err != nil {
			return entryCount, fmt.Errorf("failed to prepare insert into %q: %w", tableName, err)
		}
		for entryPair := entries.Oldest(); entryPair != nil; entryPair = entryPair.Next() {
			entry := entryPair.Value

			if err != nil {
				return entryCount, fmt.Errorf("failed to get query: %w", err)
			}

			if l.tracer.Enabled() {
				l.logger.Debug("adding query from operation to transaction", zap.Stringer("op", entry), zap.String("query", query))
			}

			values, err := convertOpToClickhouseValues(entry)
			if err != nil {
				return entryCount, fmt.Errorf("failed to get values: %w", err)
			}

			if _, err := batch.ExecContext(ctx, values...); err != nil {
				return entryCount, fmt.Errorf("executing for entry %q: %w", values, err)
			}
		}

		if err := tx.Commit(); err != nil {
			return entryCount, fmt.Errorf("failed to commit db transaction: %w", err)
		}
		entryCount += entries.Len()
	}

	return entryCount, nil
}

func (d clickhouseDialect) GetCreateCursorQuery(schema string) string {
	return fmt.Sprintf(cli.Dedent(`
	CREATE TABLE IF NOT EXISTS %s.%s
	(
    id         String,
		cursor     String,
		block_num  Int64,
		block_id   String
	) Engine = ReplacingMergeTree() ORDER BY id;
	`), EscapeIdentifier(schema), EscapeIdentifier("cursors"))
}

func (d clickhouseDialect) ExecuteSetupScript(ctx context.Context, l *Loader, schemaSql string) error {
	for _, query := range strings.Split(schemaSql, ";") {
		if len(strings.TrimSpace(query)) == 0 {
			continue
		}
		if _, err := l.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("exec schema: %w", err)
		}
	}
	return nil
}

func (d clickhouseDialect) GetUpdateCursorQuery(table, moduleHash string, cursor *sink.Cursor, block_num uint64, block_id string) string {
	return query(`
			INSERT INTO %s (id, cursor, block_num, block_id) values ('%s', '%s', %d, '%s')
	`, table, moduleHash, cursor, block_num, block_id)
}

func (d clickhouseDialect) ParseDatetimeNormalization(value string) string {
	return fmt.Sprintf("parseDateTimeBestEffort(%s)", escapeStringValue(value))
}

func (d clickhouseDialect) DriverSupportRowsAffected() bool {
	return false
}

func (d clickhouseDialect) OnlyInserts() bool {
	return true
}


func convertOpToClickhouseValues(o *Operation) ([]any, error) {
	columns := make([]string, len(o.data))
	i := 0
	for column := range o.data {
		columns[i] = column
		i++
	}
	sort.Strings(columns)
	values := make([]any, len(o.data))
	for i, v := range columns {
		convertedType, err := convertToType(o.data[v], o.table.columnsByName[v].scanType)
		if err != nil {
			return nil, fmt.Errorf("converting value %q to type %q: %w", o.data[v], o.table.columnsByName[v].scanType, err)
		}
		values[i] = convertedType
	}
	return values, nil
}


func convertToType(value string, valueType reflect.Type) (any, error) {
	switch valueType.Kind() {
	case reflect.String:
		return value, nil
	case reflect.Slice:
		return value, nil
	case reflect.Bool:
		return strconv.ParseBool(value)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.ParseInt(value, 10, 0)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint64:
		return strconv.ParseUint(value, 10, 0)
	case reflect.Uint32:
		v, err := strconv.ParseUint(value, 10, 32)
		return uint32(v), err
	case reflect.Float32, reflect.Float64:
		return strconv.ParseFloat(value, 10)
	case reflect.Struct:
		if valueType == reflectTypeTime {
			if integerRegex.MatchString(value) {
				i, err := strconv.Atoi(value)
				if err != nil {
					return "", fmt.Errorf("could not convert %s to int: %w", value, err)
				}

				return int64(i), nil
			}

			v, err := time.Parse("2006-01-02T15:04:05Z", value)
			if err != nil {
				return "", fmt.Errorf("could not convert %s to time: %w", value, err)
			}
			return v.Unix(), nil
		}
		return "", fmt.Errorf("unsupported struct type %s", valueType)

	case reflect.Ptr:
		if valueType.String() == "*big.Int" {
			newInt := new(big.Int)
			newInt.SetString(value, 10)
			return newInt, nil
		}
		return "", fmt.Errorf("unsupported pointer type %s", valueType)
	default:
		return value, nil
	}
}