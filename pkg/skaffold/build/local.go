/*
Copyright 2018 The Skaffold Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package build

import (
	"context"
	"fmt"
	"io"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/build/tag"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/constants"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/docker"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/v1alpha2"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
	"github.com/docker/docker/api/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// LocalBuilder uses the host docker daemon to build and tag the image
type LocalBuilder struct {
	*v1alpha2.LocalBuild

	api          docker.APIClient
	localCluster bool
	kubeContext  string
}

// NewLocalBuilder returns an new instance of a LocalBuilder
func NewLocalBuilder(cfg *v1alpha2.LocalBuild, kubeContext string) (*LocalBuilder, error) {
	api, err := docker.NewAPIClient()
	if err != nil {
		return nil, errors.Wrap(err, "getting docker client")
	}

	l := &LocalBuilder{
		LocalBuild: cfg,

		kubeContext:  kubeContext,
		api:          api,
		localCluster: kubeContext == constants.DefaultMinikubeContext || kubeContext == constants.DefaultDockerForDesktopContext,
	}

	if cfg.SkipPush == nil {
		logrus.Debugf("skipPush value not present. defaulting to cluster default %t (minikube=true, d4d=true, gke=false)", l.localCluster)
		cfg.SkipPush = &l.localCluster
	}

	return l, nil
}

func (l *LocalBuilder) Labels() map[string]string {
	labels := map[string]string{
		constants.Labels.Builder: "local",
	}
	v, err := l.api.ServerVersion(context.Background())
	if err == nil {
		labels[constants.Labels.DockerAPIVersion] = fmt.Sprintf("%v", v.APIVersion)
	}
	return labels
}

func (l *LocalBuilder) runBuildForArtifact(ctx context.Context, out io.Writer, artifact *v1alpha2.Artifact) (string, error) {
	switch {
	case artifact.DockerArtifact != nil:
		return l.buildDocker(ctx, out, artifact.Workspace, artifact.DockerArtifact)
	case artifact.BazelArtifact != nil:
		return l.buildBazel(ctx, out, artifact.Workspace, artifact.BazelArtifact)
	default:
		return "", fmt.Errorf("undefined artifact type: %+v", artifact.ArtifactType)
	}
}

// Build runs a docker build on the host and tags the resulting image with
// its checksum. It streams build progress to the writer argument.
func (l *LocalBuilder) Build(ctx context.Context, out io.Writer, tagger tag.Tagger, artifacts []*v1alpha2.Artifact) ([]Artifact, error) {
	if l.localCluster {
		if _, err := fmt.Fprintf(out, "Found [%s] context, using local docker daemon.\n", l.kubeContext); err != nil {
			return nil, errors.Wrap(err, "writing status")
		}
	}
	defer l.api.Close()

	var builds []Artifact

	for _, artifact := range artifacts {
		fmt.Fprintf(out, "Building [%s]...\n", artifact.ImageName)

		initialTag, err := l.runBuildForArtifact(ctx, out, artifact)
		if err != nil {
			return nil, errors.Wrapf(err, "building [%s]", artifact.ImageName)
		}

		digest, err := docker.Digest(ctx, l.api, initialTag)
		if err != nil {
			return nil, errors.Wrapf(err, "build and tag: %s", initialTag)
		}
		if digest == "" {
			return nil, fmt.Errorf("digest not found")
		}
		tag, err := tagger.GenerateFullyQualifiedImageName(artifact.Workspace, &tag.Options{
			ImageName: artifact.ImageName,
			Digest:    digest,
		})
		if err != nil {
			return nil, errors.Wrap(err, "generating tag")
		}
		if err := l.api.ImageTag(ctx, initialTag, tag); err != nil {
			return nil, errors.Wrap(err, "tagging image")
		}
		if _, err := io.WriteString(out, fmt.Sprintf("Successfully tagged %s\n", tag)); err != nil {
			return nil, errors.Wrap(err, "writing tag status")
		}
		if !*l.SkipPush {
			if err := docker.RunPush(ctx, l.api, tag, out); err != nil {
				return nil, errors.Wrapf(err, "pushing [%s]", tag)
			}
		}

		builds = append(builds, Artifact{
			ImageName: artifact.ImageName,
			Tag:       tag,
		})
	}

	return builds, nil
}

func (l *LocalBuilder) buildDocker(ctx context.Context, out io.Writer, workspace string, a *v1alpha2.DockerArtifact) (string, error) {
	initialTag := util.RandomID()

	err := docker.RunBuild(ctx, out, l.api, workspace, types.ImageBuildOptions{
		Tags:       []string{initialTag},
		Dockerfile: a.DockerfilePath,
		BuildArgs:  a.BuildArgs,
		CacheFrom:  a.CacheFrom,
	})
	if err != nil {
		return "", errors.Wrap(err, "running build")
	}

	return fmt.Sprintf("%s:latest", initialTag), nil
}
