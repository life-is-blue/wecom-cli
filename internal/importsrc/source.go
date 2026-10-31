package importsrc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// EnvSourcePassword supplies the MySQL source password when the DSN has none.
const EnvSourcePassword = "OPENETL_SOURCE_PASSWORD"

// Source yields source rows in a stable order.
type Source interface {
	// Scan calls fn once per row until it returns an error.
	Scan(ctx context.Context, fn func(Row) error) error
	// Label is a short description that never contains credentials.
	Label() string
}

// Options are the injectable inputs of Open.
type Options struct {
	Getenv func(string) string
	Stdin  io.Reader
	// Format forces "ndjson" or "csv" for file and stdin sources.
	Format string
}

// Open picks the reader for a --from value: mysql://..., "-" (NDJSON on
// stdin), or a file path (.csv is CSV; anything else is NDJSON).
func Open(from string, m *Mapping, opt Options) (Source, error) {
	if opt.Getenv == nil {
		opt.Getenv = os.Getenv
	}
	switch {
	case strings.HasPrefix(from, "mysql://"):
		return openMySQL(from, m, opt.Getenv)
	case from == "-":
		if opt.Stdin == nil {
			return nil, errors.New("no stdin available")
		}
		f := opt.Format
		if f == "" {
			f = "ndjson"
		}
		return &readerSource{label: "stdin", r: opt.Stdin, format: f}, nil
	case from == "":
		return nil, errors.New("--from is required")
	}
	f := opt.Format
	if f == "" {
		if strings.EqualFold(filepath.Ext(from), ".csv") {
			f = "csv"
		} else {
			f = "ndjson"
		}
	}
	fh, err := os.Open(from)
	if err != nil {
		return nil, err
	}
	return &readerSource{label: filepath.Base(from), r: fh, closer: fh, format: f}, nil
}

type readerSource struct {
	label  string
	r      io.Reader
	closer io.Closer
	format string
}

func (s *readerSource) Label() string { return s.label }

func (s *readerSource) Scan(ctx context.Context, fn func(Row) error) error {
	if s.closer != nil {
		defer s.closer.Close()
	}
	switch s.format {
	case "csv":
		return scanCSV(ctx, s.r, fn)
	case "ndjson", "jsonl":
		return scanNDJSON(ctx, s.r, fn)
	}
	return fmt.Errorf("unknown format %q (csv or ndjson)", s.format)
}

func scanNDJSON(ctx context.Context, r io.Reader, fn func(Row) error) error {
	br := bufio.NewReaderSize(r, 1<<20)
	line := 0
	for {
		b, err := br.ReadBytes('\n')
		if len(b) > 0 {
			line++
			if t := bytes.TrimSpace(b); len(t) > 0 {
				dec := json.NewDecoder(bytes.NewReader(t))
				dec.UseNumber()
				var row map[string]any
				if e := dec.Decode(&row); e != nil {
					return fmt.Errorf("line %d: %v", line, e)
				}
				if row == nil {
					return fmt.Errorf("line %d: not a JSON object", line)
				}
				if e := ctx.Err(); e != nil {
					return e
				}
				if e := fn(Row(row)); e != nil {
					return e
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// scanCSV reads a header row then flat records. A UTF-8 BOM is skipped.
func scanCSV(ctx context.Context, r io.Reader, fn func(Row) error) error {
	br := bufio.NewReader(r)
	if p, err := br.Peek(3); err == nil && bytes.Equal(p, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	head, err := cr.Read()
	if err != nil {
		return fmt.Errorf("csv header: %v", err)
	}
	seen := map[string]bool{}
	for i, h := range head {
		h = strings.TrimSpace(h)
		head[i] = h
		if h == "" || seen[h] {
			return fmt.Errorf("csv header: column %d is empty or duplicated (%q)", i+1, h)
		}
		seen[h] = true
	}
	for rec := 2; ; rec++ {
		fields, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("csv record %d: %v", rec, err)
		}
		if len(fields) == 1 && strings.TrimSpace(fields[0]) == "" {
			continue // blank line
		}
		if len(fields) != len(head) {
			return fmt.Errorf("csv record %d: %d fields, header has %d", rec, len(fields), len(head))
		}
		row := make(Row, len(head))
		for i, h := range head {
			row[h] = fields[i]
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := fn(row); e != nil {
			return e
		}
	}
}
