// Package images embeds the provisioning assets of the CrowdVM base images
// (see README.md). internal/node/image turns them into a NoCloud seed.
//
//	scripts/build.sh <profile>   entry point, run once by cloud-init (runcmd)
//	scripts/<step>.sh            provisioning steps
//	rootfs/<profile>/...         files copied verbatim into / (minimal first)
package images

import "embed"

// Version of the provisioning assets. Bump when the image content changes.
const Version = "4"

// Profiles in build order: "full" = "minimal" + more.
var Profiles = []string{"minimal", "full"}

//go:embed scripts rootfs
var FS embed.FS
