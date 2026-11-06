// Package k3dclient wraps the k3d library behind a small interface shaped for
// the Terraform provider. It owns the behaviors the k3d CLI normally provides:
// defaults, helper image pinning, rollback, and telling "missing" from "unreachable".
package k3dclient

import (
	"context"
	"errors"
	"fmt"
	"os"

	l "github.com/k3d-io/k3d/v5/pkg/logger"
	"github.com/k3d-io/k3d/v5/pkg/runtimes/docker"
	"github.com/k3d-io/k3d/v5/version"
	"github.com/sirupsen/logrus"
)

// LibraryVersion is the k3d module version in go.mod. k3d derives its helper
// image tags from version.Version and falls back to "latest" when it is empty.
const LibraryVersion = "v5.9.0"

// ErrNotFound reports that Docker answered and the object does not exist.
// Failures to reach Docker are always a different error.
var ErrNotFound = errors.New("not found")

func init() {
	version.Version = LibraryVersion
	l.Log().SetOutput(os.Stderr)
	l.Log().SetLevel(logrus.WarnLevel)
}

// configuredImage returns the image a container was created from. k3d's node
// data only has the resolved sha256 ID, which never matches the configured tag.
func configuredImage(ctx context.Context, containerName string) (string, error) {
	client, err := docker.GetDockerClient()
	if err != nil {
		return "", fmt.Errorf("create docker client: %w", err)
	}
	defer func() { _ = client.Close() }()

	details, err := client.ContainerInspect(ctx, containerName)
	if err != nil {
		return "", fmt.Errorf("inspect container %s: %w", containerName, err)
	}
	return details.Config.Image, nil
}
