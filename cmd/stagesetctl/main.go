// SPDX-FileCopyrightText: The stageset-controller Authors
// SPDX-License-Identifier: 0BSD

// stagesetctl is the client-side companion to the StageSet controller: it
// previews what a StageSet would change in the cluster (diff), renders a
// stage's manifests (build), forces out-of-band reconciles, and prints
// human-readable status. Placed on PATH as kubectl-stageset it is also
// invocable as `kubectl stageset …`.
package main

import (
	"context"
	"os"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/metio/stageset-controller/internal/cli"
)

// Stamped at build time via -ldflags="-X main.version=… -X main.commit=…",
// matching the controller binary's convention.
var (
	version = "development"
	commit  = "unknown"
)

func main() {
	cli.SetBuildInfo(version, commit)
	streams := genericiooptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
	os.Exit(cli.Run(context.Background(), streams, os.Args[1:]))
}
