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

package storage

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gravitational/gravity/lib/loc"

	"github.com/gravitational/teleport/lib/services"
	teleservices "github.com/gravitational/teleport/lib/services"
	teleutils "github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"k8s.io/helm/pkg/proto/hapi/release"
)

// Release represents a single instance of a running application.
type Release interface {
	// Resource is the base resource.
	services.Resource
	GetChartName() string
	GetChartVersion() loc.Locator
	GetChart() string
	GetStatus() string
	GetRevision() int
	GetUpdated() time.Time
}

// NewRelease creates a new release resource from the provided Helm release.
func NewRelease(release *release.Release) (Release, error) {
	md := release.GetChart().GetMetadata()
	chartVersion, err := loc.ParseLocator(md.GetVersion())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &ReleaseV1{
		Kind:    KindRelease,
		Version: services.V1,
		Metadata: services.Metadata{
			Name:        release.GetName(),
			Namespace:   release.GetNamespace(),
			Description: release.GetInfo().GetDescription(),
		},
		Spec: ReleaseSpecV1{
			ChartName:    md.GetName(),
			ChartVersion: *chartVersion,
			AppVersion:   md.GetAppVersion(),
		},
		Status: ReleaseStatusV1{
			Status:   release.GetInfo().GetStatus().GetCode().String(),
			Revision: int(release.GetVersion()),
			Updated:  time.Unix(release.GetInfo().GetLastDeployed().Seconds, 0),
		},
	}, nil
}

type ReleaseV1 struct {
	Kind     string            `json:"kind"`
	Version  string            `json:"version"`
	Metadata services.Metadata `json:"metadata"`
	Spec     ReleaseSpecV1     `json:"spec"`
	Status   ReleaseStatusV1   `json:"status"`
}

type ReleaseSpecV1 struct {
	ChartName    string      `json:"chart_name"`
	ChartVersion loc.Locator `json:"chart_version"`
	AppVersion   string      `json:"app_version"`
}

type ReleaseStatusV1 struct {
	Status   string    `json:"status"`
	Revision int       `json:"revision"`
	Updated  time.Time `json:"updated"`
}

func (r *ReleaseV1) GetChartName() string {
	return r.Spec.ChartName
}

func (r *ReleaseV1) GetChartVersion() loc.Locator {
	return r.Spec.ChartVersion
}

func (r *ReleaseV1) GetChart() string {
	return fmt.Sprintf("%s-%s", r.Spec.ChartName, r.Spec.ChartVersion)
}

func (r *ReleaseV1) GetStatus() string {
	return r.Status.Status
}

func (r *ReleaseV1) GetRevision() int {
	return r.Status.Revision
}

func (r *ReleaseV1) GetUpdated() time.Time {
	return r.Status.Updated
}

// GetName returns the resource name.
func (r *ReleaseV1) GetName() string {
	return r.Metadata.Name
}

// SetName sets the resource name.
func (r *ReleaseV1) SetName(name string) {
	r.Metadata.Name = name
}

// GetMetadata returns the resource metadata.
func (r *ReleaseV1) GetMetadata() services.Metadata {
	return r.Metadata
}

// SetExpiry sets the resource expiration time.
func (r *ReleaseV1) SetExpiry(expires time.Time) {
	r.Metadata.SetExpiry(expires)
}

// Expires returns the resource expiration time.
func (r *ReleaseV1) Expiry() time.Time {
	return r.Metadata.Expiry()
}

// SetTTL sets the resource TTL.
func (r *ReleaseV1) SetTTL(clock clockwork.Clock, ttl time.Duration) {
	r.Metadata.SetTTL(clock, ttl)
}

// MarshalRelease marshals provided release resource to JSON.
func MarshalRelease(release Release, opts ...teleservices.MarshalOption) ([]byte, error) {
	return json.Marshal(release)
}

// UnmarshalRelease unmarshals release resource from the provided JSON data.
func UnmarshalRelease(data []byte) (Release, error) {
	jsonData, err := teleutils.ToJSON(data)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var header teleservices.ResourceHeader
	err = json.Unmarshal(jsonData, &header)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	switch header.Version {
	case teleservices.V1:
		var release ReleaseV1
		err := teleutils.UnmarshalWithSchema(GetReleaseSchema(), &release, jsonData)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &release, nil
	}
	return nil, trace.BadParameter("%v resource version %q is not supported",
		KindRelease, header.Version)
}

// GetReleaseSchema returns the full release resource schema.
func GetReleaseSchema() string {
	return fmt.Sprintf(teleservices.V2SchemaTemplate, teleservices.MetadataSchema,
		ReleaseV1Schema, "")
}

// ReleaseV1Schema defines the release resource schema.
var ReleaseV1Schema = fmt.Sprintf(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "chart_name": {"type": "string"},
    "chart_version": {"type": "string"},
    "app_version": {"type": "string"},
  }
},
"status": {
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "status": {"type": "string"},
    "revision": {"type": "number"},
    "updated": {"type": "string"},
  }
}`)
