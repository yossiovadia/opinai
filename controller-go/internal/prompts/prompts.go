// Package prompts provides embedded AI prompt templates.
package prompts

import (
	"bytes"
	"embed"
	"text/template"
)

//go:embed *.txt
var promptFS embed.FS

// Render loads a prompt template by filename and renders it with the given data.
func Render(name string, data any) string {
	content, err := promptFS.ReadFile(name)
	if err != nil {
		return ""
	}
	tmpl, err := template.New(name).Parse(string(content))
	if err != nil {
		return string(content) // return raw if template parsing fails
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return string(content)
	}
	return buf.String()
}
