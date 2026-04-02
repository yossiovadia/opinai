package dashboard

import (
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestHTMLServed(t *testing.T) {
	srv := setupTestServer(t)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("GET / status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "OpinAI") {
		t.Error("response should contain 'OpinAI'")
	}
	if !strings.Contains(body, "<script>") {
		t.Error("response should contain '<script>'")
	}
}

func TestAdminHTMLServed(t *testing.T) {
	srv := setupTestServer(t)

	req := httptest.NewRequest("GET", "/admin", nil)
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("GET /admin status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OpinAI Admin") {
		t.Error("admin page should contain 'OpinAI Admin'")
	}
}

func TestJavaScriptSyntax(t *testing.T) {
	// Check if node is available
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available — skipping JS syntax check")
	}

	// Read embedded HTML files
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("failed to get static sub-fs: %v", err)
	}

	for _, file := range []string{"index.html", "admin.html"} {
		t.Run(file, func(t *testing.T) {
			data, err := fs.ReadFile(staticSub, file)
			if err != nil {
				t.Fatalf("failed to read %s: %v", file, err)
			}
			html := string(data)

			// Extract all <script> blocks
			scripts := extractScriptBlocks(html)
			if len(scripts) == 0 {
				t.Skipf("no <script> blocks found in %s", file)
			}

			for i, script := range scripts {
				// Write to temp file
				tmpFile, err := os.CreateTemp("", "opinai-js-*.js")
				if err != nil {
					t.Fatalf("failed to create temp file: %v", err)
				}
				tmpFile.WriteString(script)
				tmpFile.Close()
				defer os.Remove(tmpFile.Name())

				// Run node --check
				cmd := exec.Command("node", "--check", tmpFile.Name())
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Errorf("%s script block %d has syntax errors:\n%s", file, i+1, string(output))
				}
			}
		})
	}
}

// extractScriptBlocks extracts JS content from <script>...</script> tags.
func extractScriptBlocks(html string) []string {
	var blocks []string
	rest := html
	for {
		start := strings.Index(rest, "<script>")
		if start < 0 {
			break
		}
		start += len("<script>")
		end := strings.Index(rest[start:], "</script>")
		if end < 0 {
			break
		}
		block := strings.TrimSpace(rest[start : start+end])
		if len(block) > 0 {
			blocks = append(blocks, block)
		}
		rest = rest[start+end+len("</script>"):]
	}
	return blocks
}
