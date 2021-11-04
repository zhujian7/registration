package deploy

import "embed"

//go:embed spoke
//go:embed agent
var SpokeManifestFiles embed.FS
