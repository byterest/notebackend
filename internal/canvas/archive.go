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
const canvasBundleType = "infinite-note-canvas-bundle"
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

type canvasBundleManifest struct {
	Type       string                     `json:"type"`
	Version    int                        `json:"version"`
	ExportedAt string                     `json:"exportedAt"`
	Canvases   []canvasBundleManifestItem `json:"canvases"`
}

type canvasBundleManifestItem struct {
	File string `json:"file"`
	Name string `json:"name"`
}

type importedCanvas struct {
	Name string
	Data string
}

type importedArchive struct {
	items  []importedCanvas
	bundle bool
}

type fetchedAsset struct {
	path string
	data []byte
}

func (h *Handler) writeExportZip(c *gin.Context, name, data string) {
	payload := normalizeData(data)
	mapping, fetched := h.fetchAssets(c.Request.Context(), []string{payload})
	document, err := encodeCanvasDocument(name, payload, mapping)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	filename := sanitizeExportFileName(name) + ".canvas.zip"
	h.streamZip(c, filename, func(writer *zip.Writer) error {
		if err := writeZipBytes(writer, "canvas.json", document); err != nil {
			return err
		}
		return writeFetchedAssets(writer, fetched)
	})
}

func (h *Handler) writeBundleZip(c *gin.Context, items []Canvas) {
	payloads := make([]string, 0, len(items))
	for _, item := range items {
		payloads = append(payloads, normalizeData(item.Data))
	}
	mapping, fetched := h.fetchAssets(c.Request.Context(), payloads)

	exportedAt := time.Now().UTC().Format(time.RFC3339)
	manifest := canvasBundleManifest{
		Type:       canvasBundleType,
		Version:    canvasExportVersion,
		ExportedAt: exportedAt,
		Canvases:   make([]canvasBundleManifestItem, 0, len(items)),
	}
	documents := make([][]byte, 0, len(items))
	for i, item := range items {
		fileName := fmt.Sprintf("canvases/%03d.json", i+1)
		document, err := encodeCanvasDocument(item.Name, payloads[i], mapping)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		manifest.Canvases = append(manifest.Canvases, canvasBundleManifestItem{
			File: fileName,
			Name: normalizeName(item.Name),
		})
		documents = append(documents, document)
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.streamZip(c, "all-canvases.canvas.zip", func(writer *zip.Writer) error {
		if err := writeZipBytes(writer, "manifest.json", manifestBytes); err != nil {
			return err
		}
		for i, document := range documents {
			if err := writeZipBytes(writer, manifest.Canvases[i].File, document); err != nil {
				return err
			}
		}
		return writeFetchedAssets(writer, fetched)
	})
}

func (h *Handler) streamZip(c *gin.Context, filename string, write func(*zip.Writer) error) {
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", zipContentDisposition(filename))
	c.Status(http.StatusOK)

	writer := zip.NewWriter(c.Writer)
	defer writer.Close()
	_ = write(writer)
}

func (h *Handler) fetchAssets(ctx context.Context, payloads []string) (map[string]string, []fetchedAsset) {
	seen := map[string]struct{}{}
	var urls []string
	for _, payload := range payloads {
		for _, assetURL := range collectAssetURLs(payload, h.isOurAsset) {
			if _, ok := seen[assetURL]; ok {
				continue
			}
			seen[assetURL] = struct{}{}
			urls = append(urls, assetURL)
		}
	}

	fetched := make([]fetchedAsset, 0, len(urls))
	mapping := make(map[string]string, len(urls))
	for i, assetURL := range urls {
		body, err := h.readAssetBytes(ctx, assetURL)
		if err != nil || len(body) == 0 {
			continue
		}
		zipPath := zipAssetPath(i, assetURL)
		mapping[assetURL] = zipPath
		fetched = append(fetched, fetchedAsset{path: zipPath, data: body})
	}
	return mapping, fetched
}

func encodeCanvasDocument(name, payload string, mapping map[string]string) ([]byte, error) {
	rewritten := replaceMappedStrings(payload, mapping)
	used := usedAssetMapping(payload, mapping)
	return json.MarshalIndent(canvasExportDocument{
		Type:       canvasExportType,
		Version:    canvasExportVersion,
		Name:       normalizeName(name),
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Data:       json.RawMessage([]byte(rewritten)),
		Assets:     used,
	}, "", "  ")
}

func usedAssetMapping(payload string, mapping map[string]string) map[string]string {
	if len(mapping) == 0 {
		return nil
	}
	used := map[string]string{}
	for originalURL, zipPath := range mapping {
		if strings.Contains(payload, originalURL) {
			used[originalURL] = zipPath
		}
	}
	if len(used) == 0 {
		return nil
	}
	return used
}

func writeZipBytes(writer *zip.Writer, name string, data []byte) error {
	entry, err := writer.Create(name)
	if err != nil {
		return err
	}
	_, err = entry.Write(data)
	return err
}

func writeFetchedAssets(writer *zip.Writer, fetched []fetchedAsset) error {
	for _, asset := range fetched {
		if err := writeZipBytes(writer, asset.path, asset.data); err != nil {
			return err
		}
	}
	return nil
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

func (h *Handler) importCanvasFile(c *gin.Context, file io.ReadSeeker, filename string, size int64) (importedArchive, error) {
	magic := make([]byte, 4)
	if _, err := io.ReadFull(file, magic); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return importedArchive{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return importedArchive{}, err
	}

	if isZipMagic(magic) || strings.HasSuffix(strings.ToLower(filename), ".zip") {
		if ra, ok := any(file).(io.ReaderAt); ok && size > 0 {
			return h.importZip(c, ra, size)
		}
		body, err := io.ReadAll(io.LimitReader(file, h.maxImportSize+1))
		if err != nil {
			return importedArchive{}, err
		}
		if int64(len(body)) > h.maxImportSize {
			return importedArchive{}, errors.New("zip exceeds 200MB limit")
		}
		return h.importZip(c, bytes.NewReader(body), int64(len(body)))
	}

	body, err := io.ReadAll(io.LimitReader(file, h.maxImportSize+1))
	if err != nil {
		return importedArchive{}, err
	}
	if int64(len(body)) > h.maxImportSize {
		return importedArchive{}, errors.New("file exceeds 200MB limit")
	}
	name, data, err := parseImportedCanvasDocument(body, filename)
	if err != nil {
		return importedArchive{}, err
	}
	return importedArchive{items: []importedCanvas{{Name: name, Data: data}}}, nil
}

func (h *Handler) importZip(c *gin.Context, file io.ReaderAt, size int64) (importedArchive, error) {
	reader, err := zip.NewReader(file, size)
	if err != nil {
		return importedArchive{}, errors.New("invalid zip file")
	}
	if len(reader.File) > maxZipFiles {
		return importedArchive{}, errors.New("zip contains too many files")
	}

	filesByName := map[string]*zip.File{}
	assetFiles := map[string]*zip.File{}
	var canvasFiles []string
	for _, entry := range reader.File {
		name := zipEntryName(entry.Name)
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		filesByName[name] = entry
		if strings.HasPrefix(name, "assets/") {
			assetFiles[name] = entry
		}
		if strings.HasPrefix(name, "canvases/") && strings.HasSuffix(strings.ToLower(name), ".json") {
			canvasFiles = append(canvasFiles, name)
		}
	}

	replacements, err := h.uploadZipAssets(c, assetFiles)
	if err != nil {
		return importedArchive{}, err
	}

	if isBundleArchive(filesByName, canvasFiles) {
		paths := bundleCanvasPaths(filesByName, canvasFiles)
		if len(paths) == 0 {
			return importedArchive{}, errors.New("zip does not contain canvases")
		}
		items := make([]importedCanvas, 0, len(paths))
		for _, zipPath := range paths {
			entry := filesByName[zipPath]
			if entry == nil {
				return importedArchive{}, fmt.Errorf("missing %s", zipPath)
			}
			item, err := h.importZipCanvas(entry, replacements)
			if err != nil {
				return importedArchive{}, err
			}
			items = append(items, item)
		}
		return importedArchive{items: items, bundle: true}, nil
	}

	canvasFile := filesByName["canvas.json"]
	if canvasFile == nil {
		for _, entry := range reader.File {
			name := zipEntryName(entry.Name)
			if strings.HasSuffix(name, ".canvas.json") || strings.HasSuffix(strings.ToLower(name), ".json") {
				canvasFile = entry
				break
			}
		}
	}
	if canvasFile == nil {
		return importedArchive{}, errors.New("zip does not contain canvas.json")
	}
	item, err := h.importZipCanvas(canvasFile, replacements)
	if err != nil {
		return importedArchive{}, err
	}
	return importedArchive{items: []importedCanvas{item}}, nil
}

func (h *Handler) importZipCanvas(entry *zip.File, replacements map[string]string) (importedCanvas, error) {
	canvasBody, err := readZipFile(entry, h.maxImportSize)
	if err != nil {
		return importedCanvas{}, err
	}
	name, data, err := parseImportedCanvasDocument(canvasBody, entry.Name)
	if err != nil {
		return importedCanvas{}, err
	}

	merged := map[string]string{}
	for key, value := range replacements {
		merged[key] = value
	}
	var document canvasExportDocument
	if json.Unmarshal(canvasBody, &document) == nil {
		for originalURL, zipPath := range document.Assets {
			if newURL, ok := replacements[zipPath]; ok {
				merged[originalURL] = newURL
			}
		}
	}

	return importedCanvas{
		Name: name,
		Data: replaceMappedStrings(data, merged),
	}, nil
}

func (h *Handler) uploadZipAssets(c *gin.Context, assetFiles map[string]*zip.File) (map[string]string, error) {
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
			return nil, fmt.Errorf("read %s: %w", zipPath, err)
		}
		publicURL, err := h.storeImportedAsset(c, path.Base(zipPath), body)
		if err != nil {
			return nil, fmt.Errorf("store %s: %w", zipPath, err)
		}
		replacements[zipPath] = publicURL
		replacements[path.Base(zipPath)] = publicURL
	}
	return replacements, nil
}

func isBundleArchive(filesByName map[string]*zip.File, canvasFiles []string) bool {
	if len(canvasFiles) > 0 {
		return true
	}
	manifestFile := filesByName["manifest.json"]
	if manifestFile == nil {
		return false
	}
	body, err := readZipFile(manifestFile, 1<<20)
	if err != nil {
		return false
	}
	var manifest canvasBundleManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return false
	}
	return manifest.Type == canvasBundleType || len(manifest.Canvases) > 0
}

func bundleCanvasPaths(filesByName map[string]*zip.File, canvasFiles []string) []string {
	manifestFile := filesByName["manifest.json"]
	if manifestFile != nil {
		if body, err := readZipFile(manifestFile, 1<<20); err == nil {
			var manifest canvasBundleManifest
			if json.Unmarshal(body, &manifest) == nil && len(manifest.Canvases) > 0 {
				paths := make([]string, 0, len(manifest.Canvases))
				for _, item := range manifest.Canvases {
					name := zipEntryName(item.File)
					if name == "" {
						continue
					}
					paths = append(paths, name)
				}
				if len(paths) > 0 {
					return paths
				}
			}
		}
	}
	sort.Strings(canvasFiles)
	return canvasFiles
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
