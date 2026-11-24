package swagx

import (
	"encoding/json"
	"fmt"
	"net/http"

	swgui "github.com/swaggest/swgui"
	"github.com/swaggest/swgui/v5emb"
)

// defaultUITitle is used when neither the document nor the caller names the UI.
const defaultUITitle = "Swagger UI"

type documentMetadata struct {
	Swagger string `json:"swagger"`
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title string `json:"title"`
	} `json:"info"`
}

// snapshotDocument validates one rendered JSON document and returns an owned
// copy of its bytes together with the UI title derived from info.title. source
// names the document in errors.
func snapshotDocument(raw, source string) ([]byte, string, error) {
	document := []byte(raw)
	var metadata documentMetadata
	if err := json.Unmarshal(document, &metadata); err != nil {
		return nil, "", fmt.Errorf("swag: generated document %q is not valid JSON: %w", source, err)
	}
	if metadata.Swagger == "" && metadata.OpenAPI == "" {
		return nil, "", fmt.Errorf("swag: generated document %q declares neither swagger nor openapi version", source)
	}
	title := metadata.Info.Title
	if title == "" {
		title = defaultUITitle
	}
	return document, title, nil
}

// buildUIHandler prepares the embedded Swagger UI for one external mount
// point. documentURL is the path a browser requests for the JSON document, and
// uiBasePath is the external path the UI is mounted at; uiBasePath must end
// with a slash.
func buildUIHandler(title, documentURL, uiBasePath string, persistAuthorization bool) http.Handler {
	config := swgui.Config{}
	if persistAuthorization {
		config.SettingsUI = map[string]string{"persistAuthorization": "true"}
	}
	return v5emb.NewWithConfig(config)(title, documentURL, uiBasePath)
}

// writeDocument serves one JSON document with the package's response headers.
func writeDocument(w http.ResponseWriter, document []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(document)
}
