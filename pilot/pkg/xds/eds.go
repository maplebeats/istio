// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xds

import (
	"time"

	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/golang/protobuf/ptypes/any"

	networkingapi "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	networking "istio.io/istio/pilot/pkg/networking/core/v1alpha3"
	"istio.io/istio/pilot/pkg/networking/core/v1alpha3/loadbalancer"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/util/sets"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvk"
)

// UpdateServiceShards will list the endpoints and create the shards.
// This is used to reconcile and to support non-k8s registries (until they migrate).
// Note that aggregated list is expensive (for large numbers) - we want to replace
// it with a model where DiscoveryServer keeps track of all endpoint registries
// directly, and calls them one by one.
func (s *DiscoveryServer) UpdateServiceShards(push *model.PushContext) error {
	registries := s.getNonK8sRegistries()
	// Short circuit now to avoid the call to Services
	if len(registries) == 0 {
		return nil
	}
	// Each registry acts as a shard - we don't want to combine them because some
	// may individually update their endpoints incrementally
	for _, svc := range push.Services(nil) {
		for _, registry := range registries {
			// skip the service in case this svc does not belong to the registry.
			if svc.Attributes.ServiceRegistry != string(registry.Provider()) {
				continue
			}
			endpoints := make([]*model.IstioEndpoint, 0)
			for _, port := range svc.Ports {
				if port.Protocol == protocol.UDP {
					continue
				}

				// This loses track of grouping (shards)
				instances, err := registry.InstancesByPort(svc, port.Port, labels.Collection{})
				if err != nil {
					return err
				}

				for _, inst := range instances {
					endpoints = append(endpoints, inst.Endpoint)
				}
			}

			s.edsCacheUpdate(registry.Cluster(), string(svc.Hostname), svc.Attributes.Namespace, endpoints)
		}
	}

	return nil
}

// SvcUpdate is a callback from service discovery when service info changes.
func (s *DiscoveryServer) SvcUpdate(cluster, hostname string, namespace string, event model.Event) {
	// When a service deleted, we should cleanup the endpoint shards and also remove keys from EndpointShardsByService to
	// prevent memory leaks.
	if event == model.EventDelete {
		inboundServiceDeletes.Increment()
		s.deleteService(cluster, hostname, namespace)
	} else {
		inboundServiceUpdates.Increment()
	}
}

// EDSUpdate computes destination address membership across all clusters and networks.
// This is the main method implementing EDS.
// It replaces InstancesByPort in model - instead of iterating over all endpoints it uses
// the hostname-keyed map. And it avoids the conversion from Endpoint to ServiceEntry to envoy
// on each step: instead the conversion happens once, when an endpoint is first discovered.
func (s *DiscoveryServer) EDSUpdate(clusterID, serviceName string, namespace string,
	istioEndpoints []*model.IstioEndpoint) {
	inboundEDSUpdates.Increment()
	// Update the endpoint shards
	fp := s.edsCacheUpdate(clusterID, serviceName, namespace, istioEndpoints)
	// Trigger a push
	s.ConfigUpdate(&model.PushRequest{
		Full: fp,
		ConfigsUpdated: map[model.ConfigKey]struct{}{{
			Kind:      gvk.ServiceEntry,
			Name:      serviceName,
			Namespace: namespace,
		}: {}},
		Reason: []model.TriggerReason{model.EndpointUpdate},
	})
}

// EDSCacheUpdate computes destination address membership across all clusters and networks.
// This is the main method implementing EDS.
// It replaces InstancesByPort in model - instead of iterating over all endpoints it uses
// the hostname-keyed map. And it avoids the conversion from Endpoint to ServiceEntry to envoy
// on each step: instead the conversion happens once, when an endpoint is first discovered.
//
// Note: the difference with `EDSUpdate` is that it only update the cache rather than requesting a push
func (s *DiscoveryServer) EDSCacheUpdate(clusterID, serviceName string, namespace string,
	istioEndpoints []*model.IstioEndpoint) {
	inboundEDSUpdates.Increment()
	// Update the endpoint shards
	s.edsCacheUpdate(clusterID, serviceName, namespace, istioEndpoints)
}

// edsCacheUpdate updates EndpointShards data by clusterID, hostname, IstioEndpoints.
// It also tracks the changes to ServiceAccounts. It returns whether a full push
// is needed or incremental push is sufficient.
func (s *DiscoveryServer) edsCacheUpdate(clusterID, hostname string, namespace string,
	istioEndpoints []*model.IstioEndpoint) bool {
	if len(istioEndpoints) == 0 {
		// Should delete the service EndpointShards when endpoints become zero to prevent memory leak,
		// but we should not do not delete the keys from EndpointShardsByService map - that will trigger
		// unnecessary full push which can become a real problem if a pod is in crashloop and thus endpoints
		// flip flopping between 1 and 0.
		s.deleteEndpointShards(clusterID, hostname, namespace)
		adsLog.Infof("Incremental push, service %s has no endpoints", hostname)
		return false
	}

	fullPush := false

	// Find endpoint shard for this service, if it is available - otherwise create a new one.
	ep, created := s.getOrCreateEndpointShard(hostname, namespace)
	// If we create a new endpoint shard, that means we have not seen the service earlier. We should do a full push.
	if created {
		adsLog.Infof("Full push, new service %s", hostname)
		fullPush = true
	}

	// Check if ServiceAccounts have changed. We should do a full push if they have changed.
	serviceAccounts := sets.Set{}
	for _, e := range istioEndpoints {
		if e.ServiceAccount != "" {
			serviceAccounts.Insert(e.ServiceAccount)
		}
	}

	ep.mutex.Lock()
	// For existing endpoints, we need to do full push if service accounts change.
	if !fullPush && !serviceAccounts.Equals(ep.ServiceAccounts) {
		adsLog.Debugf("Updating service accounts now, svc %v, before service account %v, after %v",
			hostname, ep.ServiceAccounts, serviceAccounts)
		adsLog.Infof("Full push, service accounts changed, %v", hostname)
		fullPush = true
	}
	ep.Shards[clusterID] = istioEndpoints
	ep.ServiceAccounts = serviceAccounts
	ep.mutex.Unlock()

	return fullPush
}

func (s *DiscoveryServer) getOrCreateEndpointShard(serviceName, namespace string) (*EndpointShards, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if _, exists := s.EndpointShardsByService[serviceName]; !exists {
		s.EndpointShardsByService[serviceName] = map[string]*EndpointShards{}
	}
	if ep, exists := s.EndpointShardsByService[serviceName][namespace]; exists {
		return ep, false
	}
	// This endpoint is for a service that was not previously loaded.
	ep := &EndpointShards{
		Shards:          map[string][]*model.IstioEndpoint{},
		ServiceAccounts: sets.Set{},
	}
	s.EndpointShardsByService[serviceName][namespace] = ep

	return ep, true
}

// deleteEndpointShards deletes matching endpoint shards from EndpointShardsByService map. This is called when
// endpoints are deleted.
func (s *DiscoveryServer) deleteEndpointShards(cluster, serviceName, namespace string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.EndpointShardsByService[serviceName] != nil &&
		s.EndpointShardsByService[serviceName][namespace] != nil {
		s.EndpointShardsByService[serviceName][namespace].mutex.Lock()
		delete(s.EndpointShardsByService[serviceName][namespace].Shards, cluster)
		s.EndpointShardsByService[serviceName][namespace].mutex.Unlock()
	}
}

// deleteService deletes all service related references from EndpointShardsByService. This is called
// when a service is deleted.
func (s *DiscoveryServer) deleteService(cluster, serviceName, namespace string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.EndpointShardsByService[serviceName] != nil &&
		s.EndpointShardsByService[serviceName][namespace] != nil {

		s.EndpointShardsByService[serviceName][namespace].mutex.Lock()
		delete(s.EndpointShardsByService[serviceName][namespace].Shards, cluster)
		shards := len(s.EndpointShardsByService[serviceName][namespace].Shards)
		s.EndpointShardsByService[serviceName][namespace].mutex.Unlock()

		if shards == 0 {
			delete(s.EndpointShardsByService[serviceName], namespace)
		}
		if len(s.EndpointShardsByService[serviceName]) == 0 {
			delete(s.EndpointShardsByService, serviceName)
		}
	}
}

// loadAssignmentsForCluster return the endpoints for a cluster
// Initial implementation is computing the endpoints on the flight - caching will be added as needed, based on
// perf tests.
func (s *DiscoveryServer) loadAssignmentsForCluster(b EndpointBuilder) *endpoint.ClusterLoadAssignment {
	if b.service == nil {
		// Shouldn't happen here
		adsLog.Debugf("can not find the service for cluster %s", b.clusterName)
		return buildEmptyClusterLoadAssignment(b.clusterName)
	}

	// Service resolution type might have changed and Cluster may be still in the EDS cluster list of "Connection.Clusters".
	// This can happen if a ServiceEntry's resolution is changed from STATIC to DNS which changes the Envoy cluster type from
	// EDS to STRICT_DNS. When pushEds is called before Envoy sends the updated cluster list via Endpoint request which in turn
	// will update "Connection.Clusters", we might accidentally send EDS updates for STRICT_DNS cluster. This check guards
	// against such behavior and returns nil. When the updated cluster warms up in Envoy, it would update with new endpoints
	// automatically.
	// Gateways use EDS for Passthrough cluster. So we should allow Passthrough here.
	if b.service.Resolution == model.DNSLB {
		adsLog.Infof("cluster %s in eds cluster, but its resolution now is updated to %v, skipping it.", b.clusterName, b.service.Resolution)
		return nil
	}

	svcPort, f := b.service.Ports.GetByPort(b.port)
	if !f {
		// Shouldn't happen here
		adsLog.Debugf("can not find the service port %d for cluster %s", b.port, b.clusterName)
		return buildEmptyClusterLoadAssignment(b.clusterName)
	}

	s.mutex.RLock()
	epShards, f := s.EndpointShardsByService[string(b.hostname)][b.service.Attributes.Namespace]
	s.mutex.RUnlock()
	if !f {
		// Shouldn't happen here
		adsLog.Debugf("can not find the endpointShards for cluster %s", b.clusterName)
		return buildEmptyClusterLoadAssignment(b.clusterName)
	}

	locEps := b.buildLocalityLbEndpointsFromShards(epShards, svcPort)

	return &endpoint.ClusterLoadAssignment{
		ClusterName: b.clusterName,
		Endpoints:   locEps,
	}
}

func (s *DiscoveryServer) generateEndpoints(b EndpointBuilder) *endpoint.ClusterLoadAssignment {
	l := s.loadAssignmentsForCluster(b)
	if l == nil {
		return nil
	}

	// If networks are set (by default they aren't) apply the Split Horizon
	// EDS filter on the endpoints
	if b.push.Networks != nil && len(b.push.Networks.Networks) > 0 {
		l.Endpoints = EndpointsByNetworkFilter(b.push, b.network, l.Endpoints)
	}

	// If locality aware routing is enabled, prioritize endpoints or set their lb weight.
	// Failover should only be enabled when there is an outlier detection, otherwise Envoy
	// will never detect the hosts are unhealthy and redirect traffic.
	enableFailover, lb := getOutlierDetectionAndLoadBalancerSettings(b.destinationRule, b.port, b.subsetName)
	lbSetting := loadbalancer.GetLocalityLbSetting(b.push.Mesh.GetLocalityLbSetting(), lb.GetLocalityLbSetting())
	if lbSetting != nil {
		// Make a shallow copy of the cla as we are mutating the endpoints with priorities/weights relative to the calling proxy
		l = util.CloneClusterLoadAssignment(l)
		loadbalancer.ApplyLocalityLBSetting(b.locality, l, lbSetting, enableFailover)
	}
	return l
}

// EdsGenerator implements the new Generate method for EDS, using the in-memory, optimized endpoint
// storage in DiscoveryServer.
type EdsGenerator struct {
	Server *DiscoveryServer
}

func (eds *EdsGenerator) Generate(proxy *model.Proxy, push *model.PushContext, w *model.WatchedResource, updates model.XdsUpdates) model.Resources {
	resp := []*any.Any{}

	var edsUpdatedServices map[string]struct{} = nil
	if updates != nil {
		edsUpdatedServices = model.ConfigNamesOfKind(updates, gvk.ServiceEntry)
	}
	// All clusters that this endpoint is watching. For 1.0 - it's typically all clusters in the mesh.
	// For 1.1+Sidecar - it's the small set of explicitly imported clusters, using the isolated DestinationRules
	for _, clusterName := range w.ResourceNames {
		_, _, hostname, _ := model.ParseSubsetKey(clusterName)
		if _, f := edsUpdatedServices[string(hostname)]; f {
			continue
		}
		epb := NewEndpointBuilder(clusterName, proxy, push)
		l := eds.Server.generateEndpoints(epb)
		if l == nil {
			continue
		}
		msg := util.MessageToAny(l)
		msg.TypeUrl = w.TypeUrl
		resp = append(resp, msg)
	}

	return resp
}

// pushEds is pushing EDS updates for a single connection. Called the first time
// a client connects, for incremental updates and for full periodic updates.
func (s *DiscoveryServer) pushEds(push *model.PushContext, con *Connection, version string, edsUpdatedServices map[string]struct{}) error {
	pushStart := time.Now()
	loadAssignments := make([]*endpoint.ClusterLoadAssignment, 0)
	endpoints := 0
	empty := 0

	// All clusters that this endpoint is watching. For 1.0 - it's typically all clusters in the mesh.
	// For 1.1+Sidecar - it's the small set of explicitly imported clusters, using the isolated DestinationRules
	for _, clusterName := range con.Clusters() {
		if edsUpdatedServices != nil {
			_, _, hostname, _ := model.ParseSubsetKey(clusterName)
			if _, ok := edsUpdatedServices[string(hostname)]; !ok {
				// Cluster was not updated, skip recomputing. This happens when we get an incremental update for a
				// specific Hostname. On connect or for full push edsUpdatedServices will be empty.
				continue
			}
		}
		builder := NewEndpointBuilder(clusterName, con.proxy, push)
		l := s.generateEndpoints(builder)
		if l == nil {
			continue
		}

		for _, e := range l.Endpoints {
			endpoints += len(e.LbEndpoints)
		}

		if len(l.Endpoints) == 0 {
			empty++
		}
		loadAssignments = append(loadAssignments, l)
	}

	response := endpointDiscoveryResponse(loadAssignments, version, push.Version)
	err := con.send(response)
	edsPushTime.Record(time.Since(pushStart).Seconds())
	if err != nil {
		recordSendError("EDS", con.ConID, edsSendErrPushes, err)
		return err
	}
	edsPushes.Increment()

	if edsUpdatedServices == nil {
		adsLog.Infof("EDS: PUSH for node:%s clusters:%d endpoints:%d empty:%v",
			con.proxy.ID, len(con.Clusters()), endpoints, empty)
	} else {
		adsLog.Debugf("EDS: PUSH INC for node:%s clusters:%d endpoints:%d empty:%v",
			con.proxy.ID, len(con.Clusters()), endpoints, empty)
	}
	return nil
}

func getOutlierDetectionAndLoadBalancerSettings(
	destinationRule *networkingapi.DestinationRule,
	portNumber int,
	subsetName string) (bool, *networkingapi.LoadBalancerSettings) {
	if destinationRule == nil {
		return false, nil
	}
	var outlierDetectionEnabled = false
	var lbSettings *networkingapi.LoadBalancerSettings

	port := &model.Port{Port: portNumber}
	policy := networking.MergeTrafficPolicy(nil, destinationRule.TrafficPolicy, port)

	for _, subset := range destinationRule.Subsets {
		if subset.Name == subsetName {
			policy = networking.MergeTrafficPolicy(policy, subset.TrafficPolicy, port)
			break
		}
	}

	if policy != nil {
		lbSettings = policy.LoadBalancer
		if policy.OutlierDetection != nil {
			outlierDetectionEnabled = true
		}
	}

	return outlierDetectionEnabled, lbSettings
}

func endpointDiscoveryResponse(loadAssignments []*endpoint.ClusterLoadAssignment, version, noncePrefix string) *discovery.DiscoveryResponse {
	out := &discovery.DiscoveryResponse{
		TypeUrl: v3.EndpointType,
		// Pilot does not really care for versioning. It always supplies what's currently
		// available to it, irrespective of whether Envoy chooses to accept or reject EDS
		// responses. Pilot believes in eventual consistency and that at some point, Envoy
		// will begin seeing results it deems to be good.
		VersionInfo: version,
		Nonce:       nonce(noncePrefix),
	}
	for _, loadAssignment := range loadAssignments {
		out.Resources = append(out.Resources, util.MessageToAny(loadAssignment))
	}

	return out
}

// cluster with no endpoints
func buildEmptyClusterLoadAssignment(clusterName string) *endpoint.ClusterLoadAssignment {
	return &endpoint.ClusterLoadAssignment{
		ClusterName: clusterName,
	}
}
