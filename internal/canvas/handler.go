package canvas

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"note-backend/internal/auth"

	"github.com/gin-gonic/gin"
)

// Handler exposes canvas HTTP endpoints.
type Handler struct {
	store *Store
}

const contentChunkSize = 30
const maxDebugDelay = 2000
const canvasLockTTL = 30 * time.Second

// NewHandler creates a canvas handler.
func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

// List returns canvases for the current user.
func (h *Handler) List(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	items, err := h.store.List(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	for i := range items {
		items[i].Preview = buildPreview(items[i].Data)
		items[i].Data = ""
	}

	c.JSON(http.StatusOK, gin.H{"data": items})
}

// Create creates a new canvas for the current user.
func (h *Handler) Create(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var request struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	item, err := h.store.Create(c.Request.Context(), user.ID, normalizeName(request.Name), normalizeData(request.Data))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": item})
}

// Get returns a canvas by ID for the current user.
func (h *Handler) Get(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	item, err := h.store.Get(c.Request.Context(), user.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
}

// Stream returns a canvas in staged SSE events so the client can render skeletons first.
func (h *Handler) Stream(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	item, err := h.store.Get(c.Request.Context(), user.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	parsed, err := parseCanvasEnvelope(item.Data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid canvas data"})
		return
	}

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported"})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	debugDelay := parseDebugDelay(c.Query("debug_delay_ms"))

	if err := writeSSE(c.Writer, "meta", gin.H{
		"id":       item.ID,
		"name":     item.Name,
		"viewport": parsed.Viewport,
	}); err != nil {
		return
	}
	flusher.Flush()
	sleepIfNeeded(c, debugDelay)

	if err := writeSSE(c.Writer, "nodes:skeleton", gin.H{
		"items": buildSkeletonNodes(parsed.Nodes),
	}); err != nil {
		return
	}
	flusher.Flush()
	sleepIfNeeded(c, debugDelay)

	for _, chunk := range buildContentChunks(parsed.Nodes, contentChunkSize) {
		select {
		case <-c.Request.Context().Done():
			return
		default:
		}

		if err := writeSSE(c.Writer, "nodes:content", gin.H{"items": chunk}); err != nil {
			return
		}
		flusher.Flush()
		sleepIfNeeded(c, debugDelay)
	}

	if err := writeSSE(c.Writer, "edges", gin.H{"items": parsed.Edges}); err != nil {
		return
	}
	flusher.Flush()
	sleepIfNeeded(c, debugDelay)

	_ = writeSSE(c.Writer, "done", gin.H{})
	flusher.Flush()
}

// AcquireLock claims or renews the editing lease for a canvas.
func (h *Handler) AcquireLock(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	if _, err := h.store.Get(c.Request.Context(), user.ID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var request struct {
		ClientID string `json:"client_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	clientID := normalizeClientID(request.ClientID)
	if clientID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "client_id is required"})
		return
	}

	token, err := newLockToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create lock token"})
		return
	}

	lock, acquired, err := h.store.AcquireLock(c.Request.Context(), user.ID, id, clientID, token, canvasLockTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !acquired {
		lock.LockToken = ""
		c.JSON(http.StatusLocked, gin.H{
			"error": "canvas is open in another tab",
			"data":  lock,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": lock})
}

// Update changes canvas name or data.
func (h *Handler) Update(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	var request struct {
		Name      *string `json:"name"`
		Data      *string `json:"data"`
		ClientID  string  `json:"client_id"`
		LockToken string  `json:"lock_token"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	current, err := h.store.Get(c.Request.Context(), user.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if err := h.store.ValidateLock(c.Request.Context(), user.ID, id, normalizeClientID(request.ClientID), strings.TrimSpace(request.LockToken)); err != nil {
		if errors.Is(err, ErrInvalidLock) {
			c.JSON(http.StatusLocked, gin.H{"error": "canvas editing lock is no longer active"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	name := current.Name
	if request.Name != nil {
		name = normalizeName(*request.Name)
	}

	data := current.Data
	if request.Data != nil {
		data = normalizeData(*request.Data)
	}

	updated, err := h.store.Update(c.Request.Context(), user.ID, id, name, data)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": updated})
}

// ReleaseLock releases the editing lease held by the current client.
func (h *Handler) ReleaseLock(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	var request struct {
		ClientID  string `json:"client_id"`
		LockToken string `json:"lock_token"`
	}
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if err := h.store.ReleaseLock(c.Request.Context(), user.ID, id, normalizeClientID(request.ClientID), strings.TrimSpace(request.LockToken)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Delete removes a canvas.
func (h *Handler) Delete(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	deleted, err := h.store.Delete(c.Request.Context(), user.ID, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func parseID(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

func normalizeName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return defaultCanvasName
	}
	return name
}

func normalizeClientID(raw string) string {
	value := strings.TrimSpace(raw)
	if len(value) > 128 {
		return value[:128]
	}
	return value
}

func newLockToken() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func normalizeData(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultCanvasData
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return defaultCanvasData
	}

	if _, ok := decoded["nodes"]; !ok {
		decoded["nodes"] = []any{}
	}
	if _, ok := decoded["edges"]; !ok {
		decoded["edges"] = []any{}
	}
	if _, ok := decoded["viewport"]; !ok {
		decoded["viewport"] = map[string]any{"x": 0, "y": 0, "zoom": 1}
	}

	normalized, err := json.Marshal(decoded)
	if err != nil {
		return defaultCanvasData
	}
	return string(normalized)
}

type parsedCanvasEnvelope struct {
	Nodes    []map[string]any
	Edges    []map[string]any
	Viewport map[string]any
}

func parseCanvasEnvelope(raw string) (parsedCanvasEnvelope, error) {
	var payload struct {
		Nodes    []map[string]any `json:"nodes"`
		Edges    []map[string]any `json:"edges"`
		Viewport map[string]any   `json:"viewport"`
	}

	if err := json.Unmarshal([]byte(normalizeData(raw)), &payload); err != nil {
		return parsedCanvasEnvelope{}, err
	}

	viewport := payload.Viewport
	if viewport == nil {
		viewport = map[string]any{"x": 0, "y": 0, "zoom": 1}
	}

	return parsedCanvasEnvelope{
		Nodes:    payload.Nodes,
		Edges:    payload.Edges,
		Viewport: viewport,
	}, nil
}

func buildPreview(data string) map[string]any {
	parsed, err := parseCanvasEnvelope(data)
	if err != nil {
		return map[string]any{
			"viewport": map[string]any{"x": 0, "y": 0, "zoom": 1},
			"nodes":    []any{},
			"edges":    []any{},
		}
	}

	nodes := make([]map[string]any, 0, len(parsed.Nodes))
	for _, node := range parsed.Nodes {
		if node == nil {
			continue
		}
		id, _ := node["id"].(string)
		nodeType, _ := node["type"].(string)
		parentId, _ := node["parentId"].(string)

		var x, y float64
		if pos, ok := node["position"].(map[string]any); ok {
			x, _ = toFloat64(pos["x"])
			y, _ = toFloat64(pos["y"])
		}

		w, h := 200.0, 120.0
		if style, ok := node["style"].(map[string]any); ok {
			if sw, ok := toFloat64(style["width"]); ok && sw > 0 {
				w = sw
			}
			if sh, ok := toFloat64(style["height"]); ok && sh > 0 {
				h = sh
			}
		}

		if measured, ok := node["measured"].(map[string]any); ok {
			if mw, ok := toFloat64(measured["width"]); ok && mw > 0 {
				w = mw
			}
			if mh, ok := toFloat64(measured["height"]); ok && mh > 0 {
				h = mh
			}
		}

		previewNode := map[string]any{
			"id":   id,
			"type": nodeType,
			"x":    x,
			"y":    y,
			"w":    w,
			"h":    h,
		}
		if parentId != "" {
			previewNode["parentId"] = parentId
		}
		nodes = append(nodes, previewNode)
	}

	edges := make([]map[string]any, 0, len(parsed.Edges))
	for _, edge := range parsed.Edges {
		if edge == nil {
			continue
		}
		source, _ := edge["source"].(string)
		target, _ := edge["target"].(string)
		if source == "" || target == "" {
			continue
		}
		edges = append(edges, map[string]any{
			"source": source,
			"target": target,
		})
	}

	return map[string]any{
		"viewport": parsed.Viewport,
		"nodes":    nodes,
		"edges":    edges,
	}
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func buildSkeletonNodes(nodes []map[string]any) []map[string]any {
	items := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}

		item := map[string]any{}
		copyMapField(item, node, "id")
		copyMapField(item, node, "type")
		copyMapField(item, node, "position")
		copyMapField(item, node, "parentId")
		copyMapField(item, node, "extent")
		copyMapField(item, node, "style")
		copyMapField(item, node, "measured")
		copyMapField(item, node, "className")
		copyMapField(item, node, "zIndex")
		items = append(items, item)
	}
	return items
}

func buildContentChunks(nodes []map[string]any, chunkSize int) [][]map[string]any {
	if chunkSize <= 0 {
		chunkSize = contentChunkSize
	}

	chunks := make([][]map[string]any, 0, (len(nodes)+chunkSize-1)/chunkSize)
	current := make([]map[string]any, 0, chunkSize)

	for _, node := range nodes {
		if node == nil {
			continue
		}

		item := map[string]any{}
		copyMapField(item, node, "id")
		copyMapField(item, node, "data")
		copyMapField(item, node, "measured")

		current = append(current, item)
		if len(current) == chunkSize {
			chunks = append(chunks, current)
			current = make([]map[string]any, 0, chunkSize)
		}
	}

	if len(current) > 0 {
		chunks = append(chunks, current)
	}

	return chunks
}

func copyMapField(dst, src map[string]any, key string) {
	value, ok := src[key]
	if !ok {
		return
	}
	dst[key] = value
}

func writeSSE(writer io.Writer, event string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, encoded); err != nil {
		return err
	}
	return nil
}

func parseDebugDelay(raw string) time.Duration {
	if strings.TrimSpace(raw) == "" {
		return 0
	}

	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0
	}
	if value > maxDebugDelay {
		value = maxDebugDelay
	}
	return time.Duration(value) * time.Millisecond
}

func sleepIfNeeded(c *gin.Context, delay time.Duration) {
	if delay <= 0 {
		return
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-c.Request.Context().Done():
	case <-timer.C:
	}
}
