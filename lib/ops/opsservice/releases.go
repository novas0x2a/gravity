/*
Copyright 2018 Gravitational, Inc.

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

package opsservice

import (
	"github.com/gravitational/gravity/lib/helm"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/storage"

	"github.com/gravitational/trace"
)

// ListReleases returns all currently installed application releases
func (o *Operator) ListReleases(key ops.SiteKey) ([]storage.Release, error) {
	cluster, err := o.GetLocalSite()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// We create connection to tiller on demand to avoid keeping tunnel
	// open all the time.
	client, err := helm.NewClient(helm.ClientConfig{
		DNSAddress: cluster.DNSConfig.Addr(),
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer client.Close()
	releases, err := client.List(helm.ListParameters{})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// TODO: Fetch release endpoints as well when support for application
	// manifest is added to application images.
	return releases, nil
}
