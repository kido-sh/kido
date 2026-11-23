package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
)

// HistoryEntry is one image of a parent chain. Image is nil when the recorded parent is no longer
// there (or a newer image now has its name); Ref is then the recorded name.
type HistoryEntry struct {
	Ref       string
	Image     *engine.Image
	CreatedBy string
	Comment   string
}

// ImageHistory follows the parent chain recorded in image.json from name, newest first.
func (r *Runtime) ImageHistory(ctx context.Context, name string) ([]HistoryEntry, error) {
	ref, err := imageref.Parse(name)
	if err != nil {
		return nil, err
	}
	j, err := r.loadImage(ref)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []HistoryEntry
	for {
		seen[ref.String()] = true
		e := HistoryEntry{Ref: ref.String(), Image: j.image()}
		e.CreatedBy, e.Comment = r.historyText(ref)
		out = append(out, e)
		if j.Parent == "" {
			return out, nil
		}
		parent, err := imageref.Parse(j.Parent)
		if err != nil || seen[parent.String()] {
			return append(out, HistoryEntry{Ref: j.Parent}), nil
		}
		pj, err := r.loadImage(parent)
		if err != nil || pj.Created.After(j.Created) {
			return append(out, HistoryEntry{Ref: parent.String()}), nil
		}
		ref, j = parent, pj
	}
}

// historyText reads the optional createdBy and comment fields of image.json.
func (r *Runtime) historyText(ref imageref.Ref) (createdBy, comment string) {
	b, err := os.ReadFile(filepath.Join(ref.Dir(r.Home), "image.json"))
	if err != nil {
		return "", ""
	}
	var h struct {
		CreatedBy string `json:"createdBy"`
		Comment   string `json:"comment"`
	}
	json.Unmarshal(b, &h)
	return h.CreatedBy, h.Comment
}
