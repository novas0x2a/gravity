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

package cluster

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"strconv"

	"github.com/gravitational/gravity/lib/app"
	"github.com/gravitational/gravity/lib/archive"
	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/localenv"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/pack"
	"github.com/gravitational/gravity/lib/schema"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/update"
	"github.com/gravitational/gravity/lib/utils"
	"github.com/gravitational/rigging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	teleservices "github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// InitOperationPlan will initialize operation plan for an operation
func InitOperationPlan(
	ctx context.Context,
	localEnv, updateEnv *localenv.LocalEnvironment,
	clusterEnv *localenv.ClusterEnvironment,
	opKey ops.SiteOperationKey,
) (*storage.OperationPlan, error) {
	operation, err := storage.GetOperationByID(clusterEnv.Backend, opKey.OperationID)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if operation.Type != ops.OperationUpdate {
		return nil, trace.BadParameter("expected update operation but got %q", operation.Type)
	}

	plan, err := clusterEnv.Backend.GetOperationPlan(operation.SiteDomain, operation.ID)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	if plan != nil {
		return nil, trace.AlreadyExists("plan is already initialized")
	}

	cluster, err := clusterEnv.Operator.GetLocalSite()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	dnsConfig := cluster.DNSConfig
	if dnsConfig.IsEmpty() {
		log.Info("Detecting DNS configuration.")
		existingDNS, err := getExistingDNSConfig(localEnv.Packages)
		if err != nil {
			return nil, trace.Wrap(err, "failed to determine existing cluster DNS configuration")
		}
		dnsConfig = *existingDNS
	}

	plan, err = NewOperationPlan(PlanConfig{
		Backend:   clusterEnv.Backend,
		Apps:      clusterEnv.Apps,
		Packages:  clusterEnv.ClusterPackages,
		Client:    clusterEnv.Client,
		DNSConfig: dnsConfig,
		Operator:  clusterEnv.Operator,
		Operation: operation,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	_, err = clusterEnv.Backend.CreateOperationPlan(*plan)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return plan, nil
}

// NewOperationPlan generates a new plan for the provided operation
func NewOperationPlan(config PlanConfig) (*storage.OperationPlan, error) {
	if err := config.checkAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	servers, err := storage.GetLocalServers(config.Backend)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	servers, err = checkAndSetServerDefaults(servers, config.Client.CoreV1().Nodes())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	updateCoreDNS, err := shouldUpdateCoreDNS(config.Client)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	updateDNSAppEarly, err := shouldUpdateDNSAppEarly(config.Client)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	installedPackage, err := storage.GetLocalPackage(config.Backend)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	installedApp, err := config.Apps.GetApp(*installedPackage)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	installedRuntime, err := config.Apps.GetApp(*(installedApp.Manifest.Base()))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	updatePackage, err := config.Operation.Update.Package()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	updateApp, err := config.Apps.GetApp(*updatePackage)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	updateRuntime, err := config.Apps.GetApp(*(updateApp.Manifest.Base()))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	links, err := config.Backend.GetOpsCenterLinks(config.Operation.SiteDomain)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	trustedClusters, err := config.Backend.GetTrustedClusters()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	roles, err := config.Backend.GetRoles()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	updates, err := configUpdates(updateApp.Manifest, config.Operator,
		(*ops.SiteOperation)(config.Operation).Key(), servers)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	gravityPackage, err := updateRuntime.Manifest.Dependencies.ByName(constants.GravityPackage)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	plan, err := newOperationPlan(planConfig{
		plan: storage.OperationPlan{
			OperationID:    config.Operation.ID,
			OperationType:  config.Operation.Type,
			AccountID:      config.Operation.AccountID,
			ClusterName:    config.Operation.SiteDomain,
			Servers:        servers,
			DNSConfig:      config.DNSConfig,
			GravityPackage: *gravityPackage,
		},
		operator:          config.Operator,
		operation:         *config.Operation,
		servers:           updates,
		installedRuntime:  *installedRuntime,
		installedApp:      *installedApp,
		updateRuntime:     *updateRuntime,
		updateApp:         *updateApp,
		links:             links,
		trustedClusters:   trustedClusters,
		packageService:    config.Packages,
		shouldUpdateEtcd:  shouldUpdateEtcd,
		updateCoreDNS:     updateCoreDNS,
		dnsConfig:         config.DNSConfig,
		updateDNSAppEarly: updateDNSAppEarly,
		roles:             roles,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return plan, nil
}

func (r *PlanConfig) checkAndSetDefaults() error {
	if r.Client == nil {
		return trace.BadParameter("Kubernetes client is required")
	}
	if r.Apps == nil {
		return trace.BadParameter("application service is required")
	}
	if r.Packages == nil {
		return trace.BadParameter("package service is required")
	}
	if r.Backend == nil {
		return trace.BadParameter("backend is required")
	}
	if r.Operator == nil {
		return trace.BadParameter("cluster operator is required")
	}
	if r.Operation == nil {
		return trace.BadParameter("cluster operation is required")
	}
	return nil
}

// PlanConfig defines the configuration for creating a new operation plan
type PlanConfig struct {
	Backend   storage.Backend
	Packages  pack.PackageService
	Apps      app.Applications
	DNSConfig storage.DNSConfig
	Operator  ops.Operator
	Operation *storage.SiteOperation
	Client    *kubernetes.Clientset
}

// planConfig collects parameters needed to generate an update operation plan
type planConfig struct {
	plan     storage.OperationPlan
	operator packageRotator
	// operation is the operation to generate the plan for
	operation storage.SiteOperation
	// servers is a list of servers from cluster state
	servers []storage.ServerConfigUpdate
	// installedRuntime is the runtime of the installed app
	installedRuntime app.Application
	// installedApp is the installed app
	installedApp app.Application
	// updateRuntime is the runtime of the update app
	updateRuntime app.Application
	// updateApp is the update app
	updateApp app.Application
	// links is a list of configured remote Ops Center links
	links []storage.OpsCenterLink
	// trustedClusters is a list of configured trusted clusters
	trustedClusters []teleservices.TrustedCluster
	// packageService is a reference to the clusters package service
	packageService pack.PackageService
	// shouldUpdateEtcd returns whether we should update etcd and the versions of etcd in use
	shouldUpdateEtcd func(planConfig) (bool, string, string, error)
	// updateCoreDNS indicates whether we need to run coreDNS phase
	updateCoreDNS bool
	// dnsConfig defines the existing DNS configuration
	dnsConfig storage.DNSConfig
	// updateDNSAppEarly indicates whether we need to update the DNS app earlier than normal
	//	Only applicable for 5.3.0 -> 5.3.2
	updateDNSAppEarly bool
	// roles is the existing cluster roles
	roles []teleservices.Role
}

func newOperationPlan(p planConfig) (*storage.OperationPlan, error) {
	masters, nodes := update.SplitServers(p.servers)
	if len(masters) == 0 {
		return nil, trace.NotFound("no master servers found")
	}

	builder := phaseBuilder{}
	initPhase := *builder.init(p.installedApp.Package, p.updateApp.Package, p.servers)
	checksPhase := *builder.checks(p.installedApp.Package, p.updateApp.Package).Require(initPhase)
	preUpdatePhase := *builder.preUpdate(p.updateApp.Package).Require(initPhase)
	bootstrapPhase := *builder.bootstrap(p.servers,
		p.installedApp.Package, p.updateApp.Package).Require(initPhase)

	installedGravityPackage, err := p.installedRuntime.Manifest.Dependencies.ByName(
		constants.GravityPackage)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	supportsTaints, err := supportsTaints(*installedGravityPackage)
	if err != nil {
		log.Warnf("Failed to query support for taints/tolerations in installed runtime: %v.",
			trace.DebugReport(err))
	}
	if !supportsTaints {
		log.Debugf("No support for taints/tolerations for %v.", installedGravityPackage)
	}

	// Choose the first master node for upgrade to be the leader during the operation
	leadMaster := masters[0]

	mastersPhase := *builder.masters(leadMaster, masters[1:], supportsTaints).
		Require(checksPhase, bootstrapPhase, preUpdatePhase)
	nodesPhase := *builder.nodes(leadMaster, nodes, supportsTaints).
		Require(mastersPhase)

	allRuntimeUpdates, err := app.GetUpdatedDependencies(p.installedRuntime, p.updateRuntime)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	// some system apps may need to be skipped depending on the manifest settings
	runtimeUpdates := allRuntimeUpdates[:0]
	for _, locator := range allRuntimeUpdates {
		if !schema.ShouldSkipApp(p.updateApp.Manifest, locator) {
			runtimeUpdates = append(runtimeUpdates, locator)
		}
	}

	appUpdates, err := app.GetUpdatedDependencies(p.installedApp, p.updateApp)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// check if etcd upgrade is required or not
	updateEtcd, currentVersion, desiredVersion, err := p.shouldUpdateEtcd(p)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var root update.Phase
	root.Add(initPhase, checksPhase, preUpdatePhase)
	if len(runtimeUpdates) > 0 {
		if p.updateCoreDNS {
			corednsPhase := *builder.corednsPhase(leadMaster.Server)
			mastersPhase = *mastersPhase.Require(corednsPhase)
			root.Add(corednsPhase)
		}

		if p.updateDNSAppEarly {
			for _, update := range runtimeUpdates {
				if update.Name == constants.DNSAppPackage {
					earlyDNSAppPhase := *builder.earlyDNSApp(update)
					mastersPhase = *mastersPhase.Require(earlyDNSAppPhase)
					root.Add(earlyDNSAppPhase)
				}
			}
		}

		root.Add(bootstrapPhase, mastersPhase)
		if len(nodesPhase.Phases) > 0 {
			root.Add(nodesPhase)
		}

		if updateEtcd {
			etcdPhase := *builder.etcdPlan(leadMaster.Server, servers(masters[1:]...), servers(nodes...),
				currentVersion, desiredVersion)
			// This does not depend on previous on purpose - when the etcd block is executed,
			// remote agents might be able to sync the plan before the shutdown of etcd instances
			// has begun
			root.Add(etcdPhase)
		}

		if migrationPhase := builder.migration(leadMaster.Server, p); migrationPhase != nil {
			root.AddSequential(*migrationPhase)
		}

		// the "config" phase pulls new teleport master config packages used
		// by gravity-sites on master nodes: it needs to run *after* system
		// upgrade phase to make sure that old gravity-sites start up fine
		// in case new configuration is incompatible, but *before* runtime
		// phase so new gravity-sites can find it after they start
		configPhase := *builder.config(servers(masters...)).Require(mastersPhase)
		runtimePhase := *builder.runtime(runtimeUpdates).Require(mastersPhase)
		root.Add(configPhase, runtimePhase)
	}

	root.AddSequential(*builder.app(appUpdates), *builder.cleanup(p.servers))
	plan := p.plan
	plan.Phases = root.Phases
	update.ResolvePlan(&p.plan)

	return &plan, nil
}

// configUpdates computes the configuration updates for the specified list of servers
func configUpdates(
	manifest schema.Manifest,
	operator packageRotator,
	operation ops.SiteOperationKey,
	servers []storage.Server,
) (updates []storage.ServerConfigUpdate, err error) {
	teleportPackage, err := manifest.Dependencies.ByName(constants.TeleportPackage)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	for _, server := range servers {
		runtimePackage, err := manifest.RuntimePackageForProfile(server.Role)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		configUpdate, err := operator.RotatePlanetConfig(ops.RotatePlanetConfigRequest{
			Key:            operation,
			Server:         server,
			Manifest:       manifest,
			RuntimePackage: *runtimePackage,
			DryRun:         true,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		secretsUpdate, err := operator.RotateSecrets(ops.RotateSecretsRequest{
			AccountID:   operation.AccountID,
			ClusterName: operation.SiteDomain,
			Server:      server,
			DryRun:      true,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		masterConfig, nodeConfig, err := operator.RotateTeleportConfig(ops.RotateTeleportConfigRequest{
			Key:    operation,
			Server: server,
			DryRun: true,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		updates = append(updates, storage.ServerConfigUpdate{
			Server: server,
			Runtime: storage.RuntimeConfigUpdate{
				Package:        *runtimePackage,
				ConfigPackage:  configUpdate.Locator,
				SecretsPackage: &secretsUpdate.Locator,
			},
			Teleport: &storage.TeleportConfigUpdate{
				Package:      *teleportPackage,
				MasterConfig: masterConfig.Locator,
				NodeConfig:   nodeConfig.Locator,
			},
		})
	}
	return updates, nil
}

func checkAndSetServerDefaults(servers []storage.Server, client corev1.NodeInterface) ([]storage.Server, error) {
	nodes, err := utils.GetNodes(client)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	masterIPs := utils.GetMasters(nodes)
	// set cluster role that might have not have been set
L:
	for i, server := range servers {
		if utils.StringInSlice(masterIPs, server.AdvertiseIP) {
			servers[i].ClusterRole = string(schema.ServiceRoleMaster)
		} else {
			servers[i].ClusterRole = string(schema.ServiceRoleNode)
		}

		// Check that we're able to locate the node in the kubernetes node list
		node, ok := nodes[server.AdvertiseIP]
		if !ok {
			// The server is missing it's advertise-ip label,
			// however, if we're able to match the Nodename, our internal state is likely correct
			// and we can continue without trying to repair the Nodename
			for _, node := range nodes {
				if node.Name == server.Nodename {
					continue L
				}
			}

			return nil, trace.NotFound("unable to locate kubernetes node with label %s=%s,"+
				" please check each kubernetes node and re-add the %v label if it is missing",
				defaults.KubernetesAdvertiseIPLabel,
				server.AdvertiseIP,
				defaults.KubernetesAdvertiseIPLabel)
		}
		// Overwrite the Server Nodename with the name of the kubernetes node,
		// to fix any internal consistency issues that may occur in our internal data
		servers[i].Nodename = node.Name
	}
	return servers, nil
}

func getExistingDNSConfig(packages pack.PackageService) (*storage.DNSConfig, error) {
	_, configPackage, err := pack.FindAnyRuntimePackageWithConfig(packages)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	_, rc, err := packages.ReadPackage(*configPackage)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer rc.Close()
	var configBytes []byte
	err = archive.TarGlob(tar.NewReader(rc), "", []string{"vars.json"}, func(_ string, r io.Reader) error {
		configBytes, err = ioutil.ReadAll(r)
		if err != nil {
			return trace.Wrap(err)
		}

		return archive.Abort
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var runtimeConfig runtimeConfig
	if configBytes != nil {
		err = json.Unmarshal(configBytes, &runtimeConfig)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	dnsPort := defaults.DNSPort
	if len(runtimeConfig.DNSPort) != 0 {
		dnsPort, err = strconv.Atoi(runtimeConfig.DNSPort)
		if err != nil {
			return nil, trace.Wrap(err, "expected integer value but got %v", runtimeConfig.DNSPort)
		}
	}
	var dnsAddrs []string
	if runtimeConfig.DNSListenAddr != "" {
		dnsAddrs = append(dnsAddrs, runtimeConfig.DNSListenAddr)
	}
	dnsConfig := &storage.DNSConfig{
		Addrs: dnsAddrs,
		Port:  dnsPort,
	}
	if dnsConfig.IsEmpty() {
		*dnsConfig = storage.LegacyDNSConfig
	}
	logrus.Infof("Detected DNS configuration: %v.", dnsConfig)
	return dnsConfig, nil
}

// Only applicable for 5.3.0 -> 5.3.2
// We need to update the CoreDNS app before doing rolling restarts, because the new planet will not have embedded
// coredns, and will instead point to the kube-dns service on startup. Updating the app will deploy coredns as pods.
// TODO(knisbet) remove when 5.3.2 is no longer supported as an upgrade path
func shouldUpdateDNSAppEarly(client *kubernetes.Clientset) (bool, error) {
	_, err := client.CoreV1().Services(constants.KubeSystemNamespace).Get("kube-dns", metav1.GetOptions{})
	err = rigging.ConvertError(err)
	if err != nil {
		if trace.IsNotFound(err) {
			return true, nil
		}
		return true, trace.Wrap(err)
	}
	return false, nil
}

type runtimeConfig struct {
	// DNSListenAddr specifies the configured DNS listen address
	DNSListenAddr string `json:"PLANET_DNS_LISTEN_ADDR"`
	// DNSPort specifies the configured DNS port
	DNSPort string `json:"PLANET_DNS_PORT"`
}

type packageRotator interface {
	RotateSecrets(ops.RotateSecretsRequest) (*ops.RotatePackageResponse, error)
	RotatePlanetConfig(ops.RotatePlanetConfigRequest) (*ops.RotatePackageResponse, error)
	RotateTeleportConfig(ops.RotateTeleportConfigRequest) (*ops.RotatePackageResponse, *ops.RotatePackageResponse, error)
}
