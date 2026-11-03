package actions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// PagePublishType creates an immutable HTML page for the conversation that
// proposed it. The portfolio renders it in an isolated frame.
const PagePublishType = "page:publish"

const (
	PageTitleMax = 120
	PageHTMLMax  = 128 << 10
)

// PagePublish is page:publish's parameters.
type PagePublish struct {
	Title string `json:"title"`
	HTML  string `json:"html"`
}

func pagePublishType() Type {
	return Type{
		Name: PagePublishType,
		Description: "Publish one HTML page in this chat conversation. The page is immutable and shown only in the portfolio chat in an isolated frame. " +
			"Use inline HTML, CSS and JavaScript only; the renderer blocks external resources and script network requests, forms, popups and navigation of the parent page. The title is at most 120 bytes and HTML at most 128 KiB.",
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title": map[string]any{"type": "string", "minLength": 1, "maxLength": PageTitleMax},
				"html":  map[string]any{"type": "string", "minLength": 1, "maxLength": PageHTMLMax},
			},
			"required":             []string{"title", "html"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			_, err := DecodePagePublish(p)
			return err
		},
	}
}

// DecodePagePublish validates a page before it is recorded as an action.
func DecodePagePublish(raw json.RawMessage) (PagePublish, error) {
	var p PagePublish
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("page:publish: %w", err)
	}
	if p.Title == "" || strings.TrimSpace(p.Title) != p.Title || !utf8.ValidString(p.Title) || len(p.Title) > PageTitleMax {
		return p, fmt.Errorf("page:publish: title must be trimmed UTF-8 of 1 to %d bytes", PageTitleMax)
	}
	if p.HTML == "" || !utf8.ValidString(p.HTML) || len(p.HTML) > PageHTMLMax {
		return p, fmt.Errorf("page:publish: html must be UTF-8 of 1 to %d bytes", PageHTMLMax)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return p, fmt.Errorf("page:publish: want one JSON value")
	} else if err != io.EOF {
		return p, fmt.Errorf("page:publish: %w", err)
	}
	return p, nil
}
