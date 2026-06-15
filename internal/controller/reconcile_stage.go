// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

package controller

import (
	"strings"

	stagesv1 "github.com/metio/stageset-controller/api/v1"
)

// reconcileStageAnnotation requests a single stage re-run its actions, even
// though its pinned revision is unchanged. The value is "<stage>@<token>": the
// stage name plus an opaque token compared against that stage's
// status.stages[].lastHandledReconcileAt so the request fires exactly once.
const reconcileStageAnnotation = "stages.metio.wtf/reconcile-stage"

// parseReconcileStage extracts the requested stage and token from the
// reconcile-stage annotation. An empty stage means no (or malformed) request.
func parseReconcileStage(ss *stagesv1.StageSet) (stage, token string) {
	v := ss.Annotations[reconcileStageAnnotation]
	if v == "" {
		return "", ""
	}
	name, tok, found := strings.Cut(v, "@")
	if !found || name == "" || tok == "" {
		return "", ""
	}
	return name, tok
}
