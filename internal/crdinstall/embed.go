// Package crdinstall provides the embedded CRD YAML for the DocsPage CRD.
// The CRD is embedded into the controller binary so it can self-install on startup.
package crdinstall

import (
	_ "embed"
)

//go:embed docspage-crd.yaml
var docsPageCRDYAML []byte

// DocsPageCRD returns the raw YAML bytes for the DocsPage CRD definition.
func DocsPageCRD() []byte {
	return docsPageCRDYAML
}
