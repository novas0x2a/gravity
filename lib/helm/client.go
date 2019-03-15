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

package helm

import (
	"fmt"

	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/httplib"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/utils"
	helmutils "github.com/gravitational/gravity/lib/utils/helm"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/helm"
	"k8s.io/helm/pkg/helm/portforwarder"
	"k8s.io/helm/pkg/kube"
	"k8s.io/helm/pkg/proto/hapi/release"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
)

// Client is the Helm client.
type Client struct {
	client helm.Interface
	tunnel *kube.Tunnel
}

// ClientConfig is the Helm client configuration.
type ClientConfig struct {
	// DNSAddress is an optional in-cluster DNS address.
	DNSAddress string
	// TODO Add Helm TLS flags.
}

// NewClient returns a new Helm client instance.
func NewClient(conf ClientConfig) (*Client, error) {
	kubeClient, kubeConfig, err := getKubeClient(conf.DNSAddress)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tunnel, err := portforwarder.New("kube-system", kubeClient, kubeConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	options := []helm.Option{
		helm.Host(fmt.Sprintf("127.0.0.1:%d", tunnel.Local)),
	}
	return &Client{
		client: helm.NewClient(options...),
		tunnel: tunnel,
	}, nil
}

// InstallParameters defines Helm chart install parameters.
type InstallParameters struct {
	// Path is the Helm chart path.
	Path string
	// Values is a list of YAML files with values.
	Values []string
	// Set is a list of values set on the CLI.
	Set []string
	// Name is an optional release name.
	Name string
	// Namespace is a namespace to install release into.
	Namespace string
}

// Install installs a Helm chart and returns release information.
func (c *Client) Install(p InstallParameters) (storage.Release, error) {
	rawVals, err := helmutils.Vals(p.Values, p.Set, nil, nil, "", "", "")
	if err != nil {
		return nil, trace.Wrap(err)
	}
	chart, err := chartutil.Load(p.Path)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	response, err := c.client.InstallReleaseFromChart(
		chart, p.Namespace,
		helm.ValueOverrides(rawVals),
		helm.ReleaseName(p.Name))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	release, err := storage.NewRelease(response.GetRelease())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return release, nil
}

// ListParameters defines parameters for listing releases.
type ListParameters struct {
	// Filter is an optional release name filter as a perl regex.
	Filter string
	// All returns releases with all possible statuses.
	All bool
}

// Options returns Helm list options for these parameters.
func (p ListParameters) Options() (options []helm.ReleaseListOption) {
	if p.Filter != "" {
		options = append(options, helm.ReleaseListFilter(p.Filter))
	}
	if p.All {
		options = append(options, helm.ReleaseListStatuses(allStatuses))
	} else {
		options = append(options, helm.ReleaseListStatuses(statuses))
	}
	return options
}

// List returns list of releases matching provided parameters.
func (c *Client) List(p ListParameters) ([]storage.Release, error) {
	response, err := c.client.ListReleases(p.Options()...) // TODO Paging.
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var releases []storage.Release
	if response != nil && response.Releases != nil {
		for _, item := range response.Releases {
			release, err := storage.NewRelease(item)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			releases = append(releases, release)
		}
	}
	return releases, nil
}

// Get returns a single release with the specified name.
func (c *Client) Get(name string) (storage.Release, error) {
	releases, err := c.List(ListParameters{Filter: name})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	for _, release := range releases {
		if release.GetName() == name {
			return release, nil
		}
	}
	return nil, trace.NotFound("release %v not found", name)
}

// UpgradeParameters defines release upgrade parameters.
type UpgradeParameters struct {
	// Release is a name of the release to upgrade.
	Release string
	// Path is an upgrade chart path.
	Path string
	// Values is a list of YAML files with values.
	Values []string
	// Set is a list of values set on the CLI.
	Set []string
}

// Upgrade upgrades a release.
func (c *Client) Upgrade(p UpgradeParameters) (storage.Release, error) {
	rawVals, err := helmutils.Vals(p.Values, p.Set, nil, nil, "", "", "")
	if err != nil {
		return nil, trace.Wrap(err)
	}
	_, err = chartutil.Load(p.Path)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	response, err := c.client.UpdateRelease(
		p.Release, p.Path,
		helm.UpdateValueOverrides(rawVals))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	release, err := storage.NewRelease(response.GetRelease())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return release, nil
}

// RollbackParameters defines release rollback parameters.
type RollbackParameters struct {
	// Release is a name of the release to rollback.
	Release string
	// Revision is a revision number to rollback to.
	Revision int
}

// Rollback rolls back a release to the specified version.
func (c *Client) Rollback(p RollbackParameters) (storage.Release, error) {
	response, err := c.client.RollbackRelease(
		p.Release,
		helm.RollbackVersion(int32(p.Revision)))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	release, err := storage.NewRelease(response.Release)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return release, nil
}

// Uninstall uninstalls a release with the provided name.
func (c *Client) Uninstall(name string) (storage.Release, error) {
	response, err := c.client.DeleteRelease(name)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	release, err := storage.NewRelease(response.GetRelease())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return release, nil
}

// Revisions returns revision history for a release with the provided name.
func (c *Client) Revisions(name string) ([]storage.Release, error) {
	response, err := c.client.ReleaseHistory(name,
		helm.WithMaxHistory(maxHistory))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var releases []storage.Release
	if response != nil && response.Releases != nil {
		for _, item := range response.Releases {
			release, err := storage.NewRelease(item)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			releases = append(releases, release)
		}
	}
	return releases, nil
}

// Close closes the Helm client.
func (c *Client) Close() error {
	c.tunnel.Close()
	return nil
}

// getKubeClient returns a cluster's Kubernetes client and its config.
//
// When invoked inside a Gravity cluster, returns the cluster client. The
// dnsAddress parameter specifies the address of in-cluster DNS.
//
// Otherwise, returns a client based on the default kubeconfig.
func getKubeClient(dnsAddress string) (*kubernetes.Clientset, *rest.Config, error) {
	err := httplib.InGravity(dnsAddress)
	if err != nil {
		logrus.Infof("Not in Gravity: %v.", err)
		return utils.GetLocalKubeClient()
	}
	// Resolve the API server address in advance.
	host, err := utils.ResolveAddr(dnsAddress, fmt.Sprintf("%v:%v",
		constants.APIServerDomainName, defaults.APIServerSecurePort))
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	kubeClient, kubeConfig, err := httplib.GetClusterKubeClient(dnsAddress,
		httplib.WithHost(fmt.Sprintf("https://%v", host)))
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	return kubeClient, kubeConfig, nil
}

// statuses enumerates Helm release status codes displayed by default.
var statuses = []release.Status_Code{
	release.Status_DEPLOYED,
	release.Status_FAILED,
	release.Status_DELETING,
	release.Status_PENDING_INSTALL,
	release.Status_PENDING_UPGRADE,
	release.Status_PENDING_ROLLBACK,
}

// allStatuses enumerates all possible Helm release status codes.
var allStatuses = []release.Status_Code{
	release.Status_UNKNOWN,
	release.Status_DEPLOYED,
	release.Status_DELETED,
	release.Status_SUPERSEDED,
	release.Status_FAILED,
	release.Status_DELETING,
	release.Status_PENDING_INSTALL,
	release.Status_PENDING_UPGRADE,
	release.Status_PENDING_ROLLBACK,
}

// maxHistory is how many history revisions are returned.
const maxHistory = 256
