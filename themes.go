//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type actionAppearance struct {
	Primary string            `json:"primary"`
	Weights []int             `json:"weights"`
	Labels  map[string]string `json:"labels,omitempty"`
}
type dashboardTheme struct {
	TileOrder     []string                    `json:"tileOrder,omitempty"`
	CardOrder     []string                    `json:"cardOrder,omitempty"`
	Actions       map[string]actionAppearance `json:"actions,omitempty"`
	Version       int                         `json:"version"`
	Name          string                      `json:"name"`
	Layout        string                      `json:"layout"`
	Colors        map[string]string           `json:"colors,omitempty"`
	Columns       int                         `json:"columns,omitempty"`
	MobileColumns int                         `json:"mobileColumns,omitempty"`
}
type catalogTheme struct {
	ID string `json:"id"`
	dashboardTheme
}

var themeHex = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
var themeTokens = strings.Fields("bg panel line line-strong text muted faint accent blue blue-bg green green-bg amber amber-bg red grey")

func decodeTheme(r io.Reader) (dashboardTheme, error) {
	var t dashboardTheme
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(&t); err != nil {
		return t, err
	}
	if d.Decode(new(any)) != io.EOF {
		return t, errors.New("expected one theme object")
	}
	t.Name = strings.TrimSpace(t.Name)
	if t.Version != 1 || t.Name == "" || len(t.Name) > 80 || !slices.Contains([]string{"legacy", "new", "experimental"}, t.Layout) || t.Columns < 0 || t.Columns > 6 || t.MobileColumns < 0 || t.MobileColumns > 2 {
		return t, errors.New("check the theme version, name, layout and column counts")
	}
	sections := strings.Fields("header timer status customer financials actions message shortcuts controls commands billing played feedback")
	seen := map[string]bool{}
	for _, section := range t.CardOrder {
		if seen[section] || !slices.Contains(sections, section) {
			return t, errors.New("invalid or repeated card section")
		}
		seen[section] = true
	}
	for _, name := range t.TileOrder {
		if strings.TrimSpace(name) == "" || len(name) > 120 {
			return t, errors.New("invalid PC in tileOrder")
		}
	}
	buttons := strings.Fields("start save close")
	for state, a := range t.Actions {
		if !slices.Contains(strings.Fields("idle continuing active"), state) || !slices.Contains(buttons, a.Primary) || len(a.Weights) != 3 {
			return t, errors.New("check theme action state, primary button and three widths")
		}
		for _, n := range a.Weights {
			if n < 1 || n > 12 {
				return t, errors.New("button widths must be weights from 1 to 12")
			}
		}
		for k, v := range a.Labels {
			if !slices.Contains(buttons, k) || strings.TrimSpace(v) == "" || len(v) > 50 {
				return t, errors.New("invalid theme button label")
			}
		}
	}
	for k, v := range t.Colors {
		if !slices.Contains(themeTokens, k) || !themeHex.MatchString(v) {
			return t, fmt.Errorf("invalid theme color %q", k)
		}
	}
	return t, nil
}
func themesHandler(w http.ResponseWriter, r *http.Request) {
	dir := appDataPath("themes")
	switch r.Method {
	case http.MethodGet:
		files, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		themes := []catalogTheme{}
		for _, path := range files {
			file, err := os.Open(path)
			if err != nil {
				continue
			}
			theme, err := decodeTheme(io.LimitReader(file, 16385))
			file.Close()
			if err == nil {
				themes = append(themes, catalogTheme{"theme-" + strings.TrimSuffix(filepath.Base(path), ".json"), theme})
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, themes)
	case http.MethodPost:
		t, err := decodeTheme(http.MaxBytesReader(w, r.Body, 16384))
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		data, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		id := fmt.Sprintf("%x", sha256.Sum256(data))
		if err = os.MkdirAll(dir, 0755); err == nil {
			var file *os.File
			file, err = os.CreateTemp(dir, ".theme-*")
			if err == nil {
				defer os.Remove(file.Name())
				_, err = file.Write(data)
				closeErr := file.Close()
				if err == nil {
					err = closeErr
				}
				if err == nil {
					err = os.Rename(file.Name(), filepath.Join(dir, id+".json"))
				}
			}
		}
		if err != nil {
			http.Error(w, "could not save theme", 500)
			return
		}
		writeJSON(w, 201, catalogTheme{"theme-" + id, t})
	default:
		http.Error(w, "GET or POST required", 405)
	}
}
