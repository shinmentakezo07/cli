package managementasset

import _ "embed"

//go:embed management.html
var embeddedManagementHTML []byte

// EmbeddedManagementHTML returns the management control panel HTML bundled
// into the binary at compile time. This allows the server to serve the
// control panel without any external files or runtime downloads.
func EmbeddedManagementHTML() []byte {
	return embeddedManagementHTML
}
