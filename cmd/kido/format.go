package main

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"text/tabwriter"
	"text/template"
)

var ctrTemplateFuncs = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
	"join":  strings.Join,
	"split": strings.Split,
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"title": ctrCapitalize,
	"pad": func(s string, l, r int) string {
		return strings.Repeat(" ", l) + s + strings.Repeat(" ", r)
	},
	"truncate": func(s string, n int) string {
		if r := []rune(s); len(r) > n {
			return string(r[:n])
		}
		return s
	},
}

// ctrFormatter prints rows with --format: a Go template, "table TEMPLATE" (a header
// and aligned columns), or "json" (one JSON object per line).
type ctrFormatter struct {
	w      io.Writer
	tmpl   *template.Template
	json   bool
	table  *tabwriter.Writer
	header any
}

// ctrNewFormatter parses format; header is a value of the row type whose fields hold the column
// titles, used by "table".
func ctrNewFormatter(w io.Writer, format string, header any) (*ctrFormatter, error) {
	f := &ctrFormatter{w: w, header: header}
	format = strings.TrimSpace(format)
	if format == "json" {
		f.json = true
		return f, nil
	}
	if rest, ok := strings.CutPrefix(format, "table"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
		f.table = tabwriter.NewWriter(w, 10, 1, 3, ' ', 0)
		f.w = f.table
		format = strings.TrimSpace(rest)
		if format == "" {
			return nil, fmt.Errorf("--format table needs a template, e.g. 'table {{.ID}}\\t{{.Names}}'")
		}
	}
	format = strings.NewReplacer(`\t`, "\t", `\n`, "\n").Replace(format)
	t, err := template.New("format").Funcs(ctrTemplateFuncs).Option("missingkey=error").Parse(format)
	if err != nil {
		return nil, fmt.Errorf("--format: %w", err)
	}
	f.tmpl = t
	if f.table != nil && header != nil {
		if err := f.Row(header); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (f *ctrFormatter) Row(v any) error {
	if f.json {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(f.w, "%s\n", b)
		return err
	}
	var sb strings.Builder
	if err := f.tmpl.Execute(&sb, v); err != nil {
		return fmt.Errorf("--format: %w", err)
	}
	_, err := fmt.Fprintln(f.w, sb.String())
	return err
}

func (f *ctrFormatter) Flush() {
	if f.table != nil {
		f.table.Flush()
	}
}

// ctrHeader returns a copy of the struct type of row with each string field set to its column title:
// the field's `header` tag, or the field name in upper case.
func ctrHeader(row any) any {
	t := reflect.TypeOf(row)
	v := reflect.New(t).Elem()
	for i := range t.NumField() {
		fld := t.Field(i)
		if !fld.IsExported() || fld.Type.Kind() != reflect.String {
			continue
		}
		h := fld.Tag.Get("header")
		if h == "" {
			h = strings.ToUpper(fld.Name)
		}
		v.Field(i).SetString(h)
	}
	return v.Interface()
}
