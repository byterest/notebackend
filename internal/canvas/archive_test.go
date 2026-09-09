package canvas

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCollectAssetURLs(t *testing.T) {
	raw := `{
		"nodes": [
			{"id":"1","type":"imageNote","data":{"imageUrl":"https://s.catteryx.com/note/uploads/20260909/a.jpg"}},
			{"id":"2","type":"pdfNote","data":{"pdfUrl":"http://localhost:8081/uploads/doc.pdf"}},
			{"id":"3","type":"stickerNote","data":{"imageSrc":"data:image/png;base64,abc"}},
			{"id":"4","type":"textNote","data":{"text":"see https://example.com/x.png and https://s.catteryx.com/note/uploads/b.png"}}
		],
		"edges": [],
		"viewport": {"x":0,"y":0,"zoom":1}
	}`

	handler := &Handler{s3Endpoint: "https://s.catteryx.com"}
	urls := collectAssetURLs(raw, handler.isOurAsset)
	if len(urls) != 3 {
		t.Fatalf("collected %d urls: %v", len(urls), urls)
	}
}

func TestRewriteAssetURLsReplacesLongestFirst(t *testing.T) {
	raw := `{"data":{"imageUrl":"https://host/uploads/a.jpg"}}`
	rewritten, mapping := rewriteAssetURLs(raw, map[string]string{
		"https://host/uploads/a.jpg": "assets/001.jpg",
	})
	if mapping["https://host/uploads/a.jpg"] != "assets/001.jpg" {
		t.Fatalf("unexpected mapping: %v", mapping)
	}
	if !strings.Contains(rewritten, "assets/001.jpg") {
		t.Fatalf("rewritten json missing zip path: %s", rewritten)
	}
	if strings.Contains(rewritten, "https://host/uploads/a.jpg") {
		t.Fatalf("original url still present: %s", rewritten)
	}
}

func TestParseImportedCanvasDocument(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"type": "infinite-note-canvas",
		"name": "Roadmap",
		"data": map[string]any{
			"nodes":    []any{},
			"edges":    []any{},
			"viewport": map[string]any{"x": 1, "y": 2, "zoom": 0.5},
		},
	})

	name, data, err := parseImportedCanvasDocument(raw, "ignored.json")
	if err != nil {
		t.Fatal(err)
	}
	if name != "Roadmap" {
		t.Fatalf("name = %q", name)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["nodes"]; !ok {
		t.Fatalf("payload missing nodes: %s", data)
	}
}

func TestLocalUploadNameRejectsTraversal(t *testing.T) {
	if _, ok := localUploadName("http://localhost/uploads/../secret"); ok {
		t.Fatal("expected traversal to be rejected")
	}
	name, ok := localUploadName("https://api.example.com/uploads/nested/a.jpg?x=1")
	if !ok || name != "nested/a.jpg" {
		t.Fatalf("name=%q ok=%v", name, ok)
	}
}

func TestZipEntryNameRejectsEscape(t *testing.T) {
	if name := zipEntryName("../canvas.json"); name != "" {
		t.Fatalf("escaped path accepted: %q", name)
	}
	if name := zipEntryName("assets/001.jpg"); name != "assets/001.jpg" {
		t.Fatalf("normal path rejected: %q", name)
	}
}
