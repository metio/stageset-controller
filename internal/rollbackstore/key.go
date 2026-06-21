// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package rollbackstore

import (
	"fmt"
	"strings"
)

// Key addresses a stage's rendered output in the store:
// "<namespace>/<name>/<stage>/<digest>", with ':' replaced by '-' so the
// algo:hex digest is safe as a path segment and an object name. The controller
// (writer) and the MCP diff_revisions tool (reader) both derive the key here, so
// the addressing stays a single contract neither side can drift from.
func Key(namespace, name, stage, digest string) string {
	return fmt.Sprintf("%s/%s/%s/%s", namespace, name, stage, strings.ReplaceAll(digest, ":", "-"))
}
