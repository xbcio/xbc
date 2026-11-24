package swagx

import (
	"fmt"
	"net/http"
	"path"
	"strings"

	swaggo "github.com/swaggo/swag"
)

// defaultDocumentName is the file name the standalone handler serves the JSON
// document under unless WithDocumentName overrides it.
const defaultDocumentName = "doc.json"

// HandlerOption configures a standalone documentation handler built by
// NewHandler.
type HandlerOption func(*handlerConfig)

type handlerConfig struct {
	basePath             string
	documentName         string
	title                string
	persistAuthorization bool
}

// WithBasePath declares the URL prefix the handler is mounted under, for
// example "/swagger" or "/api/v1/docs". The zero value mounts at the server
// root. The handler uses the prefix to build the document URL embedded in the
// UI and to recognise requests that arrive with the prefix still attached.
func WithBasePath(prefix string) HandlerOption {
	return func(cfg *handlerConfig) { cfg.basePath = prefix }
}

// WithDocumentName sets the JSON document's file name beneath the mount
// prefix. The default is "doc.json", so a handler mounted at "/swagger" serves
// it at "/swagger/doc.json".
func WithDocumentName(name string) HandlerOption {
	return func(cfg *handlerConfig) { cfg.documentName = name }
}

// WithTitle overrides the UI title derived from the document's info.title.
func WithTitle(title string) HandlerOption {
	return func(cfg *handlerConfig) { cfg.title = title }
}

// WithPersistAuthorization keeps Swagger UI's authorization entries across
// page reloads.
func WithPersistAuthorization(persist bool) HandlerOption {
	return func(cfg *handlerConfig) { cfg.persistAuthorization = persist }
}

// standaloneHandler serves one immutable JSON document and its embedded
// Swagger UI independently of the Web transport's route table.
type standaloneHandler struct {
	document     []byte
	documentPath string
	basePath     string
	uiBasePath   string
	ui           http.Handler
}

// NewHandler builds an http.Handler that serves an embedded Swagger UI and the
// JSON document it describes. Any net/http compatible stack can mount it,
// unlike the Plugin form, which registers routes on the Web transport.
//
// The handler serves GET and HEAD only. The mount root ("/") and "/index.html"
// return the UI, the document name (default "doc.json") returns the JSON, the
// embedded UI assets are served beneath the same prefix, and every other path
// is not found. Mount it under a prefix and tell it that prefix with
// WithBasePath:
//
//	handler, err := swagx.NewHandler(docs.SwaggerInfo, swagx.WithBasePath("/swagger"))
//	if err != nil {
//		return err
//	}
//	mux.Handle("/swagger/", http.StripPrefix("/swagger", handler))
//
// The returned handler is read-only and snapshots the document once; later
// mutations of the generated SwaggerInfo do not change served bytes.
func NewHandler(document swaggo.Swagger, opts ...HandlerOption) (http.Handler, error) {
	if document == nil {
		return nil, fmt.Errorf("swag: generated document is nil")
	}

	cfg := handlerConfig{documentName: defaultDocumentName}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	basePath, err := normalizeBasePath(cfg.basePath)
	if err != nil {
		return nil, err
	}
	documentName, err := normalizeDocumentName(cfg.documentName)
	if err != nil {
		return nil, err
	}
	snapshot, title, err := snapshotDocument(document.ReadDoc(), "standalone handler")
	if err != nil {
		return nil, err
	}
	if trimmed := strings.TrimSpace(cfg.title); trimmed != "" {
		title = trimmed
	}

	uiBasePath := basePath + "/"
	return &standaloneHandler{
		document:     snapshot,
		documentPath: "/" + documentName,
		basePath:     basePath,
		uiBasePath:   uiBasePath,
		ui:           buildUIHandler(title, uiBasePath+documentName, uiBasePath, cfg.persistAuthorization),
	}, nil
}

func (h *standaloneHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	switch relative := h.relativePath(r.URL.Path); relative {
	case "", "/", "/index.html":
		h.forward(w, r, h.uiBasePath)
	case h.documentPath:
		writeDocument(w, h.document)
	default:
		h.forward(w, r, h.basePath+relative)
	}
}

// relativePath maps a request path to the handler's own namespace. A caller may
// mount the handler with http.StripPrefix, so a path that does not carry the
// configured prefix is already relative and passes through unchanged.
func (h *standaloneHandler) relativePath(requestPath string) string {
	if requestPath == "" || requestPath[0] != '/' {
		requestPath = "/" + requestPath
	}
	if h.basePath == "" {
		return requestPath
	}
	if requestPath == h.basePath {
		return "/"
	}
	if strings.HasPrefix(requestPath, h.basePath+"/") {
		return strings.TrimPrefix(requestPath, h.basePath)
	}
	return requestPath
}

// forward serves an embedded UI page or asset. The UI handler routes on the
// external path, so the request carries the mount prefix even when the caller
// stripped it.
func (h *standaloneHandler) forward(w http.ResponseWriter, r *http.Request, externalPath string) {
	request := r.Clone(r.Context())
	request.URL.Path = externalPath
	request.URL.RawPath = ""
	h.ui.ServeHTTP(w, request)
}

// normalizeBasePath validates a handler mount prefix and returns it without a
// trailing slash; the root mount is the empty string.
func normalizeBasePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "/" {
		return "", nil
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("swag: handler base path %q must be an absolute path", value)
	}
	if strings.ContainsAny(value, ":*?#") {
		return "", fmt.Errorf("swag: handler base path %q must be a static path", value)
	}
	clean := path.Clean(value)
	if clean == "/" {
		return "", nil
	}
	return clean, nil
}

// normalizeDocumentName validates the JSON file name served beneath the mount
// prefix. It must be a single static path segment that cannot shadow the UI
// index.
func normalizeDocumentName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("swag: document name cannot be empty")
	}
	if strings.ContainsAny(value, "/\\?#") {
		return "", fmt.Errorf("swag: document name %q must be a single path segment", value)
	}
	switch strings.ToLower(value) {
	case ".", "..":
		return "", fmt.Errorf("swag: document name cannot be %q", value)
	case "index.html":
		return "", fmt.Errorf("swag: document name %q conflicts with the UI index", value)
	}
	return value, nil
}
