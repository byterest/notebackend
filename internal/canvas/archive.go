package canvas

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	notes3 "note-backend/internal/s3"

	"github.com/gin-gonic/gin"
)

const canvasExportType = "infinite-note-canvas"
const canvasExportVersion = 2

var embeddedURLPattern = regexp.MustCompile(`https?://[^\s"'<>\\)]+|/(?:uploads)/[^\s"'<>\\)]+`)

type canvasExportDocument struct {
	Type       string            `json:"type"`
	Version    int               `json:"version"`
	Name       string            `json:"name"`
	ExportedAt string            `json:"exportedAt"`
	Data       json.RawMessage   `json:"data"`
	Assets     map[string]string `json:"assets,omitempty"`
}

func (h *Handler) writeExportZip(c *gin.Context, name, data string) {
	payload := normalizeData(data)
	assetURLs := collectAssetURLs(payload, h.isOurAsset)

	type fetchedAsset struct {
		path string
		data []byte
	}
	fetched := make([]fetchedAsset, 0, len(assetURLs))
	mapping := make(map[string]string, len(assetURLs))
	for i, assetURL := range assetURLs {
		body, err := h.readAssetBytes(c.Request.Context(), assetURL)
		if err != nil || len(body) == 0 {
			continue
		}
		zipPath := zipAssetPath(i, assetURL)
		mapping[assetURL] = zipPath
		fetched = append(fetched, fetchedAsset{path: zipPath, data: body})
	}

	rewritten := replaceMappedStrings(payload, mapping)
	document, err := json.MarshalIndent(canvasExportDocument{
		Type:       canvasExportType,
		Version:    canvasExportVersion,
		Name:       normalizeName(name),
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Data:       json.RawMessage([]byte(rewritten)),
		Assets:     invertStringMap(mapping),
	}, "", "  ")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	filename := sanitizeExportFileName(name) + ".canvas.zip"
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", zipContentDisposition(filename))
	c.Status(http.StatusOK)

	writer := zip.NewWriter(c.Writer)
	defer writer.Close()

	jsonEntry, err := writer.Create("canvas.json")
	if err != nil {
		return
	}
	if _, err := jsonEntry.Write(document); err != nil {
		return
	}

	for _, asset := range fetched {
		entry, err := writer.Create(asset.path)
		if err != nil {
			return
		}
		if _, err := entry.Write(asset.data); err != nil {
			return
		}
	}
}

func (h *Handler) readAssetBytes(ctx context.Context, assetURL string) ([]byte, error) {
	body, err := h.openAsset(ctx, assetURL)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxUncompressedAsset+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxUncompressedAsset {
		return nil, errors.New("asset is too large")
	}
	return data, nil
}

func (h *Handler) importCanvasFile(c *gin.Context, file io.ReadSeeker, filename string, size int64) (string, string, error) {
	magic := make([]byte, 4)
	if _, err := io.ReadFull(file, magic); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", err
	}

	if isZipMagic(magic) || strings.HasSuffix(strings.ToLower(filename), ".zip") {
		if ra, ok := any(file).(io.ReaderAt); ok && size > 0 {
			return h.importZip(c, ra, size)
		}
		body, err := io.ReadAll(io.LimitReader(file, h.maxImportSize+1))
		if err != nil {
			return "", "", err
		}
		if int64(len(body)) > h.maxImportSize {
			return "", "", errors.New("zip exceeds 200MB limit")
		}
		return h.importZip(c, bytes.NewReader(body), int64(len(body)))
	}

	body, err := io.ReadAll(io.LimitReader(file, h.maxImportSize+1))
	if err != nil {
		return "", "", err
	}
	if int64(len(body)) > h.maxImportSize {
		return "", "", errors.New("file exceeds 200MB limit")
	}
	return parseImportedCanvasDocument(body, filename)
}

func (h *Handler) importZip(c *gin.Context, file io.ReaderAt, size int64) (string, string, error) {
	reader, err := zip.NewReader(file, size)
	if err != nil {
		return "", "", errors.New("invalid zip file")
	}
	if len(reader.File) > maxZipFiles {
		return "", "", errors.New("zip contains too many files")
	}

	var canvasFile *zip.File
	assetFiles := make(map[string]*zip.File, len(reader.File))
	for _, entry := range reader.File {
		name := zipEntryName(entry.Name)
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		if name == "canvas.json" || strings.HasSuffix(name, ".canvas.json") {
			canvasFile = entry
			continue
		}
		if strings.HasPrefix(name, "assets/") {
			assetFiles[name] = entry
		}
	}
	if canvasFile == nil {
		for _, entry := range reader.File {
			name := zipEntryName(entry.Name)
			if strings.HasSuffix(strings.ToLower(name), ".json") {
				canvasFile = entry
				break
			}
		}
	}
	if canvasFile == nil {
		return "", "", errors.New("zip does not contain canvas.json")
	}

	canvasBody, err := readZipFile(canvasFile, h.maxImportSize)
	if err != nil {
		return "", "", err
	}
	name, data, err := parseImportedCanvasDocument(canvasBody, canvasFile.Name)
	if err != nil {
		return "", "", err
	}

	replacements := map[string]string{}
	keys := make([]string, 0, len(assetFiles))
	for key := range assetFiles {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, zipPath := range keys {
		entry := assetFiles[zipPath]
		body, err := readZipFile(entry, maxUncompressedAsset)
		if err != nil {
			return "", "", fmt.Errorf("read %s: %w", zipPath, err)
		}
		publicURL, err := h.storeImportedAsset(c, path.Base(zipPath), body)
		if err != nil {
			return "", "", fmt.Errorf("store %s: %w", zipPath, err)
		}
		replacements[zipPath] = publicURL
		replacements[path.Base(zipPath)] = publicURL
	}

	var document canvasExportDocument
	if json.Unmarshal(canvasBody, &document) == nil {
		for originalURL, zipPath := range document.Assets {
			if newURL, ok := replacements[zipPath]; ok {
				replacements[originalURL] = newURL
			}
		}
	}

	return name, replaceMappedStrings(data, replacements), nil
}

func (h *Handler) storeImportedAsset(c *gin.Context, filename string, data []byte) (string, error) {
	safeName := sanitizeExportFileName(strings.TrimSuffix(filename, filepath.Ext(filename)))
	ext := strings.ToLower(filepath.Ext(filename))
	if safeName == "" {
		safeName = "asset"
	}
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}

	if h.s3Client != nil {
		key := notes3.GenerateKey("uploads", safeName+ext)
		if _, err := h.s3Client.Upload(c.Request.Context(), key, bytes.NewReader(data), contentType); err != nil {
			return "", err
		}
		return h.s3Client.PublicURL(h.s3Endpoint, key), nil
	}

	if h.uploadDir == "" {
		return "", errors.New("upload directory is not configured")
	}
	if err := os.MkdirAll(h.uploadDir, 0o755); err != nil {
		return "", err
	}
	storedName := fmt.Sprintf("%d%s", time.Now().UnixNano(), ext)
	if err := os.WriteFile(filepath.Join(h.uploadDir, storedName), data, 0o644); err != nil {
		return "", err
	}
	return publicUploadsURL(c, storedName), nil
}

func publicUploadsURL(c *gin.Context, filename string) string {
	host := c.Request.Host
	if forwardedHost := c.GetHeader("X-Forwarded-Host"); forwardedHost != "" {
		host = forwardedHost
	}
	proto := "http"
	if c.Request.TLS != nil {
		proto = "https"
	}
	if forwardedProto := c.GetHeader("X-Forwarded-Proto"); forwardedProto != "" {
		proto = forwardedProto
	}
	return fmt.Sprintf("%s://%s/uploads/%s", proto, host, filename)
}

func (h *Handler) openAsset(ctx context.Context, assetURL string) (io.ReadCloser, error) {
	if key, ok := h.s3KeyFromURL(assetURL); ok && h.s3Client != nil {
		body, _, err := h.s3Client.Download(ctx, key)
		if err == nil {
			return body, nil
		}
	}

	if name, ok := localUploadName(assetURL); ok && h.uploadDir != "" {
		file, err := os.Open(filepath.Join(h.uploadDir, name))
		if err == nil {
			return file, nil
		}
	}

	if strings.HasPrefix(assetURL, "http://") || strings.HasPrefix(assetURL, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := h.httpDo(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, fmt.Errorf("download failed: %s", resp.Status)
		}
		return resp.Body, nil
	}

	return nil, fmt.Errorf("unable to open asset %s", assetURL)
}

func (h *Handler) httpDo(req *http.Request) (*http.Response, error) {
	client := h.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}

func (h *Handler) isOurAsset(raw string) bool {
	value := strings.TrimSpace(raw)
	if value == "" || strings.HasPrefix(value, "data:") || strings.HasPrefix(value, "blob:") {
		return false
	}
	if strings.Contains(value, "/uploads/") {
		return true
	}
	if h.s3Endpoint != "" && strings.HasPrefix(value, h.s3Endpoint+"/") {
		return true
	}
	return false
}

func (h *Handler) s3KeyFromURL(raw string) (string, bool) {
	if h.s3Endpoint == "" || h.s3Bucket == "" {
		return "", false
	}
	prefix := h.s3Endpoint + "/" + h.s3Bucket + "/"
	if !strings.HasPrefix(raw, prefix) {
		return "", false
	}
	key := strings.TrimPrefix(raw, prefix)
	if idx := strings.IndexAny(key, "?#"); idx >= 0 {
		key = key[:idx]
	}
	key = strings.TrimLeft(key, "/")
	if key == "" || strings.Contains(key, "..") {
		return "", false
	}
	return key, true
}

func collectAssetURLs(raw string, isAsset func(string) bool) []string {
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil
	}

	seen := map[string]struct{}{}
	var urls []string
	walkStrings(decoded, func(value string) {
		for _, match := range embeddedURLPattern.FindAllString(value, -1) {
			assetURL := strings.TrimRight(match, ".,;")
			if !isAsset(assetURL) {
				continue
			}
			if _, ok := seen[assetURL]; ok {
				continue
			}
			seen[assetURL] = struct{}{}
			urls = append(urls, assetURL)
		}
	})
	return urls
}

func zipAssetPath(index int, assetURL string) string {
	ext := strings.ToLower(filepath.Ext(assetFileName(assetURL)))
	if ext == "" {
		ext = ".bin"
	}
	return fmt.Sprintf("assets/%03d%s", index+1, ext)
}

func rewriteAssetURLs(raw string, mapping map[string]string) (string, map[string]string) {
	if len(mapping) == 0 {
		return raw, mapping
	}
	return replaceMappedStrings(raw, mapping), mapping
}

func replaceMappedStrings(raw string, mapping map[string]string) string {
	if len(mapping) == 0 {
		return raw
	}
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return len(keys[i]) > len(keys[j])
	})
	replaced := raw
	for _, key := range keys {
		replaced = strings.ReplaceAll(replaced, key, mapping[key])
	}
	return replaced
}

func parseImportedCanvasDocument(raw []byte, fallbackName string) (string, string, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", "", errors.New("the selected file does not contain canvas data")
	}

	object, _ := decoded.(map[string]any)
	candidates := make([]importedCandidate, 0, 4)
	if object != nil {
		if canvasValue, ok := object["canvas"].(map[string]any); ok {
			candidates = append(candidates, importedCandidate{
				name:  firstNonEmptyString(canvasValue["name"], object["name"]),
				value: firstPresent(canvasValue["data"], canvasValue),
			})
		}
		if _, ok := object["data"]; ok {
			candidates = append(candidates, importedCandidate{
				name:  asString(object["name"]),
				value: object["data"],
			})
		}
	}
	candidates = append(candidates, importedCandidate{
		name:  asString(mapValue(object, "name")),
		value: decoded,
	})

	for _, candidate := range candidates {
		payload, ok := asCanvasPayload(candidate.value)
		if !ok {
			continue
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		return normalizeImportedCanvasName(candidate.name, fallbackName), string(encoded), nil
	}

	return "", "", errors.New("the selected file does not contain canvas data")
}

type importedCandidate struct {
	name  string
	value any
}

func asCanvasPayload(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case string:
		var decoded any
		if err := json.Unmarshal([]byte(typed), &decoded); err != nil {
			return nil, false
		}
		return asCanvasPayload(decoded)
	case map[string]any:
		_, hasNodes := typed["nodes"]
		_, hasEdges := typed["edges"]
		_, hasViewport := typed["viewport"]
		if !hasNodes && !hasEdges && !hasViewport {
			return nil, false
		}
		payload := map[string]any{
			"nodes":    typed["nodes"],
			"edges":    typed["edges"],
			"viewport": typed["viewport"],
		}
		if payload["nodes"] == nil {
			payload["nodes"] = []any{}
		}
		if payload["edges"] == nil {
			payload["edges"] = []any{}
		}
		if payload["viewport"] == nil {
			payload["viewport"] = map[string]any{"x": 0, "y": 0, "zoom": 1}
		}
		return payload, true
	default:
		return nil, false
	}
}

func walkStrings(value any, fn func(string)) {
	switch typed := value.(type) {
	case string:
		fn(typed)
	case []any:
		for _, item := range typed {
			walkStrings(item, fn)
		}
	case map[string]any:
		for _, item := range typed {
			walkStrings(item, fn)
		}
	}
}

func localUploadName(rawURL string) (string, bool) {
	idx := strings.Index(rawURL, "/uploads/")
	if idx < 0 {
		return "", false
	}
	name := rawURL[idx+len("/uploads/"):]
	if q := strings.IndexAny(name, "?#"); q >= 0 {
		name = name[:q]
	}
	cleaned := path.Clean("/uploads/" + name)
	if cleaned == "/uploads" || !strings.HasPrefix(cleaned, "/uploads/") {
		return "", false
	}
	return strings.TrimPrefix(cleaned, "/uploads/"), true
}

func assetFileName(rawURL string) string {
	trimmed := rawURL
	if q := strings.IndexAny(trimmed, "?#"); q >= 0 {
		trimmed = trimmed[:q]
	}
	parsed, err := url.Parse(trimmed)
	if err == nil && parsed.Path != "" {
		return path.Base(parsed.Path)
	}
	return path.Base(trimmed)
}

func zipEntryName(raw string) string {
	name := strings.ReplaceAll(raw, "\\", "/")
	name = path.Clean(name)
	name = strings.TrimPrefix(name, "/")
	if name == "." || strings.HasPrefix(name, "../") || strings.Contains(name, ":") {
		return ""
	}
	return name
}

func readZipFile(file *zip.File, limit int64) ([]byte, error) {
	if file.UncompressedSize64 > uint64(limit) {
		return nil, errors.New("file is too large")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("file is too large")
	}
	return body, nil
}

func isZipMagic(magic []byte) bool {
	return len(magic) >= 4 && magic[0] == 0x50 && magic[1] == 0x4b && (magic[2] == 0x03 || magic[2] == 0x05 || magic[2] == 0x07) && (magic[3] == 0x04 || magic[3] == 0x06 || magic[3] == 0x08)
}

func sanitizeExportFileName(name string) string {
	trimmed := strings.TrimSpace(name)
	var builder strings.Builder
	builder.Grow(len(trimmed))
	for _, r := range trimmed {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			builder.WriteByte('-')
			continue
		}
		if unicode.IsSpace(r) {
			builder.WriteByte(' ')
			continue
		}
		builder.WriteRune(r)
	}
	cleaned := strings.TrimSpace(strings.Join(strings.Fields(builder.String()), " "))
	if cleaned == "" {
		return "Untitled Canvas"
	}
	if len(cleaned) > 80 {
		return strings.TrimSpace(cleaned[:80])
	}
	return cleaned
}

func zipContentDisposition(filename string) string {
	escaped := url.PathEscape(filename)
	ascii := strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII || r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '-'
		}
		return r
	}, filename)
	if ascii == "" {
		ascii = "canvas.zip"
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, escaped)
}

func invertStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func normalizeImportedCanvasName(name, fallback string) string {
	cleaned := strings.TrimSpace(name)
	cleaned = strings.TrimSuffix(cleaned, ".canvas.zip")
	cleaned = strings.TrimSuffix(cleaned, ".zip")
	cleaned = strings.TrimSuffix(cleaned, ".canvas.json")
	cleaned = strings.TrimSuffix(cleaned, ".json")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned != "" {
		return cleaned
	}
	return normalizeName(strings.TrimSuffix(strings.TrimSuffix(fallback, filepath.Ext(fallback)), ".canvas"))
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if text := asString(value); text != "" {
			return text
		}
	}
	return ""
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func mapValue(object map[string]any, key string) any {
	if object == nil {
		return nil
	}
	return object[key]
}

func firstPresent(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
