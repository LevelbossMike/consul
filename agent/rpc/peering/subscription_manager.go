package peering

import (
	"context"
	"fmt"
	"strings"

	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/go-hclog"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/cache"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/agent/submatview"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/proto/pbcommon"
	"github.com/hashicorp/consul/proto/pbservice"
)

type MaterializedViewStore interface {
	Get(ctx context.Context, req submatview.Request) (submatview.Result, error)
	Notify(ctx context.Context, req submatview.Request, cID string, ch chan<- cache.UpdateEvent) error
}

type SubscriptionBackend interface {
	Subscriber
	Store() Store
}

// subscriptionManager handlers requests to subscribe to events from an events publisher.
type subscriptionManager struct {
	logger    hclog.Logger
	viewStore MaterializedViewStore
	backend   SubscriptionBackend
}

// TODO(peering): Maybe centralize so that there is a single manager per datacenter, rather than per peering.
func newSubscriptionManager(ctx context.Context, logger hclog.Logger, backend SubscriptionBackend) *subscriptionManager {
	logger = logger.Named("subscriptions")
	store := submatview.NewStore(logger.Named("viewstore"))
	go store.Run(ctx)

	return &subscriptionManager{
		logger:    logger,
		viewStore: store,
		backend:   backend,
	}
}

// subscribe returns a channel that will contain updates to exported service instances for a given peer.
func (m *subscriptionManager) subscribe(ctx context.Context, peerID, partition string) <-chan cache.UpdateEvent {
	var (
		updateCh       = make(chan cache.UpdateEvent, 1)
		publicUpdateCh = make(chan cache.UpdateEvent, 1)
	)

	state := newSubscriptionState(partition)
	state.publicUpdateCh = publicUpdateCh
	state.updateCh = updateCh

	// Wrap our bare state store queries in goroutines that emit events.
	go m.notifyExportedServicesForPeerID(ctx, state, peerID)
	go m.notifyMeshGatewaysForPartition(ctx, state, state.partition)

	// This goroutine is the only one allowed to manipulate protected
	// subscriptionManager fields.
	go m.handleEvents(ctx, state, updateCh)

	return publicUpdateCh
}

func (m *subscriptionManager) handleEvents(ctx context.Context, state *subscriptionState, updateCh <-chan cache.UpdateEvent) {
	for {
		// TODO(peering): exponential backoff

		select {
		case <-ctx.Done():
			return
		case update := <-updateCh:
			if err := m.handleEvent(ctx, state, update); err != nil {
				m.logger.Error("Failed to handle update from watch",
					"id", update.CorrelationID, "error", err,
				)
				continue
			}
		}
	}
}

func (m *subscriptionManager) handleEvent(ctx context.Context, state *subscriptionState, u cache.UpdateEvent) error {
	if u.Err != nil {
		return fmt.Errorf("received error event: %w", u.Err)
	}

	// TODO(peering): on initial stream setup, transmit the list of exported
	// services for use in differential DELETE/UPSERT. Akin to streaming's snapshot start/end.
	switch {
	case u.CorrelationID == subExportedServiceList:
		// Everything starts with the exported service list coming from
		// our state store watchset loop.
		evt, ok := u.Result.(*structs.ExportedServiceList)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		state.exportList = evt

		pending := &pendingPayload{}
		m.syncNormalServices(ctx, state, pending, evt.Services)
		m.syncDiscoveryChains(ctx, state, pending, evt.ListAllDiscoveryChains())
		state.sendPendingEvents(ctx, m.logger, pending)

		// cleanup event versions too
		state.cleanupEventVersions(m.logger)

	case strings.HasPrefix(u.CorrelationID, subExportedService):
		csn, ok := u.Result.(*pbservice.IndexedCheckServiceNodes)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		// TODO(peering): is it safe to edit these protobufs in place?

		// Clear this raft index before exporting.
		csn.Index = 0

		// Ensure that connect things are scrubbed so we don't mix-and-match
		// with the synthetic entries that point to mesh gateways.
		filterConnectReferences(csn)

		// Flatten health checks
		for _, instance := range csn.Nodes {
			instance.Checks = flattenChecks(
				instance.Node.Node,
				instance.Service.ID,
				instance.Service.Service,
				instance.Service.EnterpriseMeta,
				instance.Checks,
			)
		}

		id := servicePayloadIDPrefix + strings.TrimPrefix(u.CorrelationID, subExportedService)

		// Just ferry this one directly along to the destination.
		pending := &pendingPayload{}
		if err := pending.Add(id, u.CorrelationID, csn); err != nil {
			return err
		}
		state.sendPendingEvents(ctx, m.logger, pending)

	case strings.HasPrefix(u.CorrelationID, subMeshGateway):
		csn, ok := u.Result.(*pbservice.IndexedCheckServiceNodes)
		if !ok {
			return fmt.Errorf("invalid type for response: %T", u.Result)
		}

		partition := strings.TrimPrefix(u.CorrelationID, subMeshGateway)

		if !acl.EqualPartitions(partition, state.partition) {
			return nil // ignore event
		}

		// Clear this raft index before exporting.
		csn.Index = 0

		state.meshGateway = csn

		pending := &pendingPayload{}

		// Directly replicate information about our mesh gateways to the consuming side.
		// TODO(peering): should we scrub anything before replicating this?
		if err := pending.Add(meshGatewayPayloadID, u.CorrelationID, csn); err != nil {
			return err
		}

		if state.exportList != nil {
			// Trigger public events for all synthetic discovery chain replies.
			for chainName := range state.connectServices {
				m.emitEventForDiscoveryChain(ctx, state, pending, chainName)
			}
		}

		// TODO(peering): should we ship this down verbatim to the consumer?
		state.sendPendingEvents(ctx, m.logger, pending)

	default:
		return fmt.Errorf("unknown correlation ID: %s", u.CorrelationID)
	}
	return nil
}

func filterConnectReferences(orig *pbservice.IndexedCheckServiceNodes) {
	newNodes := make([]*pbservice.CheckServiceNode, 0, len(orig.Nodes))
	for i := range orig.Nodes {
		csn := orig.Nodes[i]

		if csn.Service.Kind != string(structs.ServiceKindTypical) {
			continue // skip non-typical services
		}

		if strings.HasSuffix(csn.Service.Service, syntheticProxyNameSuffix) {
			// Skip things that might LOOK like a proxy so we don't get a
			// collision with the ones we generate.
			continue
		}

		// Remove connect things like native mode.
		if csn.Service.Connect != nil || csn.Service.Proxy != nil {
			csn = proto.Clone(csn).(*pbservice.CheckServiceNode)
			csn.Service.Connect = nil
			csn.Service.Proxy = nil
		}

		newNodes = append(newNodes, csn)
	}
	orig.Nodes = newNodes
}

func (m *subscriptionManager) syncNormalServices(
	ctx context.Context,
	state *subscriptionState,
	pending *pendingPayload,
	services []structs.ServiceName,
) {
	// seen contains the set of exported service names and is used to reconcile the list of watched services.
	seen := make(map[structs.ServiceName]struct{})

	// Ensure there is a subscription for each service exported to the peer.
	for _, svc := range services {
		seen[svc] = struct{}{}

		if _, ok := state.watchedServices[svc]; ok {
			// Exported service is already being watched, nothing to do.
			continue
		}

		notifyCtx, cancel := context.WithCancel(ctx)
		if err := m.NotifyStandardService(notifyCtx, svc, state.updateCh); err != nil {
			cancel()
			m.logger.Error("failed to subscribe to service", "service", svc.String())
			continue
		}

		state.watchedServices[svc] = cancel
	}

	// For every subscription without an exported service, call the associated cancel fn.
	for svc, cancel := range state.watchedServices {
		if _, ok := seen[svc]; !ok {
			cancel()

			delete(state.watchedServices, svc)

			// Send an empty event to the stream handler to trigger sending a DELETE message.
			// Cancelling the subscription context above is necessary, but does not yield a useful signal on its own.
			err := pending.Add(
				servicePayloadIDPrefix+svc.String(),
				subExportedService+svc.String(),
				&pbservice.IndexedCheckServiceNodes{},
			)
			if err != nil {
				m.logger.Error("failed to send event for service", "service", svc.String(), "error", err)
				continue
			}
		}
	}
}

func (m *subscriptionManager) syncDiscoveryChains(
	ctx context.Context,
	state *subscriptionState,
	pending *pendingPayload,
	chainsByName map[structs.ServiceName]struct{},
) {
	// if it was newly added, then try to emit an UPDATE event
	for chainName := range chainsByName {
		if _, ok := state.connectServices[chainName]; ok {
			continue
		}

		state.connectServices[chainName] = struct{}{}

		m.emitEventForDiscoveryChain(ctx, state, pending, chainName)
	}

	// if it was dropped, try to emit an DELETE event
	for chainName := range state.connectServices {
		if _, ok := chainsByName[chainName]; ok {
			continue
		}

		delete(state.connectServices, chainName)

		if state.meshGateway != nil {
			// Only need to clean this up if we know we may have ever sent it in the first place.
			proxyName := generateProxyNameForDiscoveryChain(chainName)
			err := pending.Add(
				discoveryChainPayloadIDPrefix+chainName.String(),
				subExportedService+proxyName.String(),
				&pbservice.IndexedCheckServiceNodes{},
			)
			if err != nil {
				m.logger.Error("failed to send event for discovery chain", "service", chainName.String(), "error", err)
				continue
			}
		}
	}
}

func (m *subscriptionManager) emitEventForDiscoveryChain(
	ctx context.Context,
	state *subscriptionState,
	pending *pendingPayload,
	chainName structs.ServiceName,
) {
	if _, ok := state.connectServices[chainName]; !ok {
		return // not found
	}

	if state.exportList == nil || state.meshGateway == nil {
		return // skip because we don't have the data to do it yet
	}

	// Emit event with fake data
	proxyName := generateProxyNameForDiscoveryChain(chainName)

	err := pending.Add(
		discoveryChainPayloadIDPrefix+chainName.String(),
		subExportedService+proxyName.String(),
		createDiscoChainHealth(
			chainName,
			state.meshGateway,
		),
	)
	if err != nil {
		m.logger.Error("failed to send event for discovery chain", "service", chainName.String(), "error", err)
	}
}

func createDiscoChainHealth(sn structs.ServiceName, pb *pbservice.IndexedCheckServiceNodes) *pbservice.IndexedCheckServiceNodes {
	fakeProxyName := sn.Name + syntheticProxyNameSuffix

	newNodes := make([]*pbservice.CheckServiceNode, 0, len(pb.Nodes))
	for i := range pb.Nodes {
		gwNode := pb.Nodes[i].Node
		gwService := pb.Nodes[i].Service
		gwChecks := pb.Nodes[i].Checks

		pbEntMeta := pbcommon.NewEnterpriseMetaFromStructs(sn.EnterpriseMeta)

		fakeProxyID := fakeProxyName
		if gwService.ID != "" {
			// This is only going to be relevant if multiple mesh gateways are
			// on the same exporting node.
			fakeProxyID = fmt.Sprintf("%s-instance-%d", fakeProxyName, i)
		}

		csn := &pbservice.CheckServiceNode{
			Node: gwNode,
			Service: &pbservice.NodeService{
				Kind:           string(structs.ServiceKindConnectProxy),
				Service:        fakeProxyName,
				ID:             fakeProxyID,
				EnterpriseMeta: pbEntMeta,
				PeerName:       structs.DefaultPeerKeyword,
				Proxy: &pbservice.ConnectProxyConfig{
					DestinationServiceName: sn.Name,
					DestinationServiceID:   sn.Name,
				},
				// direct
				Address:         gwService.Address,
				TaggedAddresses: gwService.TaggedAddresses,
				Port:            gwService.Port,
				SocketPath:      gwService.SocketPath,
				Weights:         gwService.Weights,
			},
			Checks: flattenChecks(gwNode.Node, fakeProxyID, fakeProxyName, pbEntMeta, gwChecks),
		}
		newNodes = append(newNodes, csn)
	}

	return &pbservice.IndexedCheckServiceNodes{
		Index: 0,
		Nodes: newNodes,
	}
}

func flattenChecks(
	nodeName string,
	serviceID string,
	serviceName string,
	entMeta *pbcommon.EnterpriseMeta,
	checks []*pbservice.HealthCheck,
) []*pbservice.HealthCheck {
	if len(checks) == 0 {
		return nil
	}

	healthStatus := api.HealthPassing
	for _, chk := range checks {
		if chk.Status != api.HealthPassing {
			healthStatus = chk.Status
		}
	}

	if serviceID == "" {
		serviceID = serviceName
	}

	return []*pbservice.HealthCheck{
		{
			CheckID:        serviceID + ":overall-check",
			Name:           "overall-check",
			Status:         healthStatus,
			Node:           nodeName,
			ServiceID:      serviceID,
			ServiceName:    serviceName,
			EnterpriseMeta: entMeta,
			PeerName:       structs.DefaultPeerKeyword,
		},
	}
}

const (
	subExportedServiceList = "exported-service-list"
	subExportedService     = "exported-service:"
	subMeshGateway         = "mesh-gateway:"
)

// NotifyStandardService will notify the given channel when there are updates
// to the requested service of the same name in the catalog.
func (m *subscriptionManager) NotifyStandardService(
	ctx context.Context,
	svc structs.ServiceName,
	updateCh chan<- cache.UpdateEvent,
) error {
	sr := newExportedStandardServiceRequest(m.logger, svc, m.backend)
	return m.viewStore.Notify(ctx, sr, subExportedService+svc.String(), updateCh)
}

// syntheticProxyNameSuffix is the suffix to add to synthetic proxies we
// replicate to route traffic to an exported discovery chain through the mesh
// gateways.
//
// This name was chosen to match existing "sidecar service" generation logic
// and similar logic in the Service Identity synthetic ACL policies.
const syntheticProxyNameSuffix = "-sidecar-proxy"

func generateProxyNameForDiscoveryChain(sn structs.ServiceName) structs.ServiceName {
	return structs.NewServiceName(sn.Name+syntheticProxyNameSuffix, &sn.EnterpriseMeta)
}
