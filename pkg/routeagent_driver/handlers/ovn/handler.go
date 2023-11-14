/*
SPDX-License-Identifier: Apache-2.0

Copyright Contributors to the Submariner project.

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

package ovn

import (
	"net"
	"sync"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/pkg/errors"
	"github.com/submariner-io/admiral/pkg/log"
	"github.com/submariner-io/admiral/pkg/watcher"
	submV1 "github.com/submariner-io/submariner/pkg/apis/submariner.io/v1"
	"github.com/submariner-io/submariner/pkg/cable/wireguard"
	"github.com/submariner-io/submariner/pkg/cidr"
	clientset "github.com/submariner-io/submariner/pkg/client/clientset/versioned"
	"github.com/submariner-io/submariner/pkg/cni"
	"github.com/submariner-io/submariner/pkg/event"
	"github.com/submariner-io/submariner/pkg/iptables"
	"github.com/submariner-io/submariner/pkg/netlink"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type NewOVSDBClientFn func(_ model.ClientDBModel, _ ...libovsdbclient.Option) (libovsdbclient.Client, error)

type HandlerConfig struct {
	Namespace      string
	ClusterCIDR    []string
	ServiceCIDR    []string
	SubmClient     clientset.Interface
	K8sClient      kubernetes.Interface
	DynClient      dynamic.Interface
	WatcherConfig  *watcher.Config
	NewOVSDBClient NewOVSDBClientFn
}

type Handler struct {
	event.HandlerBase
	HandlerConfig
	mutex                     sync.Mutex
	cableRoutingInterface     *net.Interface
	remoteEndpoints           map[string]*submV1.Endpoint
	isGateway                 bool
	netLink                   netlink.Interface
	ipt                       iptables.Interface
	gatewayRouteController    *GatewayRouteController
	nonGatewayRouteController *NonGatewayRouteController
	stopCh                    chan struct{}
}

var logger = log.Logger{Logger: logf.Log.WithName("OVN")}

func NewHandler(config *HandlerConfig) *Handler {
	// We'll panic if env is nil, this is intentional
	ipt, err := iptables.New()
	if err != nil {
		logger.Fatalf("Error initializing iptables in OVN routeagent handler: %s", err)
	}

	h := &Handler{
		HandlerConfig:   *config,
		remoteEndpoints: map[string]*submV1.Endpoint{},
		netLink:         netlink.New(),
		ipt:             ipt,
		stopCh:          make(chan struct{}),
	}

	if h.NewOVSDBClient == nil {
		h.NewOVSDBClient = libovsdbclient.NewOVSDBClient
	}

	return h
}

func (ovn *Handler) GetName() string {
	return "ovn-hostroutes-handler"
}

func (ovn *Handler) GetNetworkPlugins() []string {
	return []string{cni.OVNKubernetes}
}

func (ovn *Handler) Init() error {
	ovn.LegacyCleanup()

	err := ovn.initIPtablesChains()
	if err != nil {
		return err
	}

	ovn.startRouteConfigSyncer(ovn.stopCh)

	connectionHandler := NewConnectionHandler(ovn.K8sClient, ovn.DynClient)

	err = connectionHandler.initClients(ovn.NewOVSDBClient)
	if err != nil {
		return errors.Wrapf(err, "error getting connection handler to connect to OvnDB")
	}

	gatewayRouteController, err := NewGatewayRouteController(*ovn.WatcherConfig, connectionHandler, ovn.Namespace)
	if err != nil {
		return err
	}

	ovn.gatewayRouteController = gatewayRouteController

	if err != nil {
		return err
	}

	nonGatewayRouteController, err := NewNonGatewayRouteController(*ovn.WatcherConfig, connectionHandler, ovn.K8sClient, ovn.Namespace)
	if err != nil {
		return err
	}

	ovn.nonGatewayRouteController = nonGatewayRouteController

	return err
}

func (ovn *Handler) LocalEndpointCreated(endpoint *submV1.Endpoint) error {
	var routingInterface *net.Interface
	var err error

	// TODO: this logic belongs to the cabledrivers instead
	if endpoint.Spec.Backend == "wireguard" {
		// NOTE: This assumes that LocalEndpointCreated happens before than TransitionToGatewayNode
		if routingInterface, err = net.InterfaceByName(wireguard.DefaultDeviceName); err != nil {
			return errors.Wrapf(err, "Wireguard interface %s not found on the node", wireguard.DefaultDeviceName)
		}
	} else {
		if routingInterface, err = netlink.GetDefaultGatewayInterface(); err != nil {
			logger.Fatalf("Unable to find the default interface on host: %s", err.Error())
		}
	}

	ovn.mutex.Lock()
	defer ovn.mutex.Unlock()

	ovn.cableRoutingInterface = routingInterface

	return nil
}

func (ovn *Handler) RemoteEndpointCreated(endpoint *submV1.Endpoint) error {
	if err := cidr.OverlappingSubnets(ovn.ServiceCIDR, ovn.ClusterCIDR,
		endpoint.Spec.Subnets); err != nil {
		// Skip processing the endpoint when CIDRs overlap and return nil to avoid re-queuing.
		logger.Errorf(err, "overlappingSubnets for new remote %#v returned error", endpoint)
		return nil
	}

	ovn.mutex.Lock()
	defer ovn.mutex.Unlock()

	ovn.remoteEndpoints[endpoint.Name] = endpoint

	err := ovn.updateHostNetworkDataplane()
	if err != nil {
		return errors.Wrapf(err, "updateHostNetworkDataplane returned error")
	}

	if ovn.isGateway {
		for _, subnet := range endpoint.Spec.Subnets {
			if err = ovn.addNoMasqueradeIPTables(subnet); err != nil {
				return errors.Wrapf(err, "error adding no-masquerade rules for subnet %q", subnet)
			}
		}

		return ovn.updateGatewayDataplane()
	}

	return nil
}

func (ovn *Handler) RemoteEndpointUpdated(endpoint *submV1.Endpoint) error {
	if err := cidr.OverlappingSubnets(ovn.ServiceCIDR, ovn.ClusterCIDR, endpoint.Spec.Subnets); err != nil {
		// Skip processing the endpoint when CIDRs overlap and return nil to avoid re-queuing.
		logger.Errorf(err, "overlappingSubnets for new remote %#v returned error", endpoint)
		return nil
	}

	ovn.mutex.Lock()
	defer ovn.mutex.Unlock()

	ovn.remoteEndpoints[endpoint.Name] = endpoint

	err := ovn.updateHostNetworkDataplane()
	if err != nil {
		return errors.Wrapf(err, "updateHostNetworkDataplane returned error")
	}

	if ovn.isGateway {
		return ovn.updateGatewayDataplane()
	}

	return nil
}

func (ovn *Handler) RemoteEndpointRemoved(endpoint *submV1.Endpoint) error {
	ovn.mutex.Lock()
	defer ovn.mutex.Unlock()

	delete(ovn.remoteEndpoints, endpoint.Name)

	err := ovn.updateHostNetworkDataplane()
	if err != nil {
		return errors.Wrapf(err, "updateHostNetworkDataplane returned error")
	}

	if ovn.isGateway {
		for _, subnet := range endpoint.Spec.Subnets {
			if err = ovn.removeNoMasqueradeIPTables(subnet); err != nil {
				return errors.Wrapf(err, "error removing no-masquerade rules for subnet %q", subnet)
			}
		}

		return ovn.updateGatewayDataplane()
	}

	return nil
}

func (ovn *Handler) TransitionToNonGateway() error {
	ovn.mutex.Lock()
	defer ovn.mutex.Unlock()

	ovn.isGateway = false
	for _, endpoint := range ovn.remoteEndpoints {
		for _, subnet := range endpoint.Spec.Subnets {
			if err := ovn.removeNoMasqueradeIPTables(subnet); err != nil {
				return errors.Wrapf(err, "error removing no-masquerade rules for subnet %q", subnet)
			}
		}
	}

	return ovn.cleanupGatewayDataplane()
}

func (ovn *Handler) TransitionToGateway() error {
	ovn.mutex.Lock()
	defer ovn.mutex.Unlock()

	ovn.isGateway = true

	for _, endpoint := range ovn.remoteEndpoints {
		for _, subnet := range endpoint.Spec.Subnets {
			if err := ovn.addNoMasqueradeIPTables(subnet); err != nil {
				return errors.Wrapf(err, "error adding no-masquerade rules for subnet %q", subnet)
			}
		}
	}

	return ovn.updateGatewayDataplane()
}
