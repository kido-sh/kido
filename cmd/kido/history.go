package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("history", "Show the history of an image", ctrHistory)
}

type ctrHistoryRow struct {
	ID           string `header:"IMAGE"`
	CreatedSince string `header:"CREATED"`
	CreatedAt    string `header:"CREATED AT"`
	CreatedBy    string `header:"CREATED BY"`
	Size         string
	Comment      string
}

// ctrHistoryRows turns an image's parent chain into rows; a parent that is gone is <missing>.
func ctrHistoryRows(hist []runtime.HistoryEntry, human, trunc bool) []ctrHistoryRow {
	var rows []ctrHistoryRow
	for _, h := range hist {
		if h.Image == nil {
			rows = append(rows, ctrHistoryRow{ID: "<missing>"})
			continue
		}
		row := ctrHistoryRow{
			ID:        ctrImageID(h.Image),
			CreatedAt: h.Image.Created.Local().Format(time.RFC3339),
			CreatedBy: runtime.Sanitize(strings.ReplaceAll(h.CreatedBy, "\t", " ")),
			Comment:   runtime.Sanitize(h.Comment),
		}
		row.CreatedSince, row.Size = row.CreatedAt, fmt.Sprint(h.Image.Size)
		if human {
			row.CreatedSince, row.Size = ctrAgo(h.Image.Created)+" ago", ctrSize(h.Image.Size)
		}
		if trunc {
			row.ID = row.ID[:12]
			if r := []rune(row.CreatedBy); len(r) > 45 {
				row.CreatedBy = string(r[:44]) + "…"
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func ctrHistory(ctx context.Context, args []string) int {
	fs := ctrFlags("history", "[OPTIONS] IMAGE")
	human := fs.Bool("H", true, "print sizes and dates in human readable format")
	fs.BoolVar(human, "human", true, "print sizes and dates in human readable format")
	noTrunc := fs.Bool("no-trunc", false, "don't truncate output")
	quiet := fs.Bool("q", false, "only show image IDs")
	fs.BoolVar(quiet, "quiet", false, "only show image IDs")
	format := fs.String("format", "", "format output with a Go template, 'table TEMPLATE' or 'json'")
	if err := fs.Parse(ctrExpand(ctrInterspersed(fs, args), "q", "format")); err != nil {
		return 125
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	refs, err := ctrImageRefs(ctx, rt, fs.Arg(0))
	if err != nil {
		return ctrFail(err)
	}
	hist, err := rt.ImageHistory(ctx, refs[0])
	if err != nil {
		return ctrFail(err)
	}
	rows := ctrHistoryRows(hist, *human, !*noTrunc)
	if *quiet && *format == "" {
		for _, r := range rows {
			fmt.Println(r.ID)
		}
		return 0
	}
	if *format == "" {
		*format = "table {{.ID}}\t{{.CreatedSince}}\t{{.CreatedBy}}\t{{.Size}}\t{{.Comment}}"
	}
	f, err := ctrNewFormatter(os.Stdout, *format, ctrHeader(ctrHistoryRow{}))
	if err != nil {
		return ctrFail(err)
	}
	defer f.Flush()
	for _, r := range rows {
		if err := f.Row(r); err != nil {
			return ctrFail(err)
		}
	}
	return 0
}
