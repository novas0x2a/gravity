/*
Copyright 2019 Gravitational, Inc.

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

package phases

import (
	"context"
	"io"

	"github.com/gravitational/gravity/lib/app"
	libfsm "github.com/gravitational/gravity/lib/fsm"
	"github.com/gravitational/gravity/lib/loc"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/pack"
	"github.com/gravitational/gravity/lib/schema"
	"github.com/gravitational/gravity/lib/storage"

	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
)

// NewUpdateConfig returns a new executor to update runtime configuration on the specified node
func NewUpdateConfig(
	params libfsm.ExecutorParams,
	operator operator,
	operation ops.SiteOperation,
	apps appGetter,
	packages packageService,
	logger log.FieldLogger,
) (*updateConfig, error) {
	if params.Phase.Data == nil || params.Phase.Data.Package == nil {
		return nil, trace.NotFound("no installed application package specified for phase %q",
			params.Phase.ID)
	}
	app, err := apps.GetApp(*params.Phase.Data.Package)
	if err != nil {
		return nil, trace.Wrap(err, "failed to query installed application")
	}
	servers := params.Plan.Servers
	if params.Phase.Data.Update != nil && len(params.Phase.Data.Update.Servers) != 0 {
		servers = params.Phase.Data.Update.Servers
	}
	return &updateConfig{
		FieldLogger: logger,
		operator:    operator,
		operation:   operation,
		packages:    packages,
		servers:     servers,
		manifest:    app.Manifest,
	}, nil
}

// Execute generates new runtime configuration with the specified environment
func (r *updateConfig) Execute(ctx context.Context) error {
	for _, server := range r.servers {
		r.Infof("Generate new runtime configuration package for %v.", server)
		runtimePackage, err := r.manifest.RuntimePackageForProfile(server.Role)
		if err != nil {
			return trace.Wrap(err)
		}
		req := ops.RotatePlanetConfigRequest{
			Key:      r.operation.Key(),
			Server:   server,
			Manifest: r.manifest,
			Package:  *runtimePackage,
			Env:      r.operation.UpdateEnviron.Env,
		}
		resp, err := r.operator.RotatePlanetConfig(req)
		if err != nil {
			return trace.Wrap(err)
		}
		_, err = r.packages.UpsertPackage(resp.Locator, resp.Reader,
			pack.WithLabels(resp.Labels))
		if err != nil {
			return trace.Wrap(err)
		}
	}
	err := r.operator.UpdateClusterEnvironmentVariables(ops.UpdateClusterEnvironRequest{
		ClusterKey: r.operation.ClusterKey(),
		Env:        r.operation.UpdateEnviron.Env,
	})
	return trace.Wrap(err)
}

// Rollback resets the cluster configuration to the previous value
func (r *updateConfig) Rollback(context.Context) error {
	err := r.operator.UpdateClusterEnvironmentVariables(ops.UpdateClusterEnvironRequest{
		ClusterKey: r.operation.ClusterKey(),
		Env:        r.operation.UpdateEnviron.PrevEnv,
	})
	return trace.Wrap(err)
}

// PreCheck is a no-op
func (r *updateConfig) PreCheck(context.Context) error {
	return nil
}

// PostCheck is a no-op
func (r *updateConfig) PostCheck(context.Context) error {
	return nil
}

type updateConfig struct {
	// FieldLogger specifies the logger for the phase
	log.FieldLogger
	operator  operator
	operation ops.SiteOperation
	packages  packageService
	servers   []storage.Server
	manifest  schema.Manifest
}

type operator interface {
	RotatePlanetConfig(ops.RotatePlanetConfigRequest) (*ops.RotatePackageResponse, error)
	UpdateClusterEnvironmentVariables(ops.UpdateClusterEnvironRequest) error
}

type appGetter interface {
	GetApp(loc.Locator) (*app.Application, error)
}

type packageService interface {
	UpsertPackage(loc.Locator, io.Reader, ...pack.PackageOption) (*pack.PackageEnvelope, error)
	DeletePackage(loc.Locator) error
}
