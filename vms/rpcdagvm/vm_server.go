// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package rpcdagvm

import (
	"context"
	"encoding/json"
	"fmt"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/ava-labs/avalanchego/api/keystore/gkeystore"
	"github.com/ava-labs/avalanchego/api/metrics"
	"github.com/ava-labs/avalanchego/chains/atomic/gsharedmemory"
	"github.com/ava-labs/avalanchego/database/corruptabledb"
	"github.com/ava-labs/avalanchego/database/manager"
	"github.com/ava-labs/avalanchego/database/rpcdb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/ids/galiasreader"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/engine/avalanche/vertex"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/common/appsender"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/avalanchego/vms/rpcdagvm/ghttp"
	"github.com/ava-labs/avalanchego/vms/rpcdagvm/grpcutils"
	"github.com/ava-labs/avalanchego/vms/rpcdagvm/gsubnetlookup"
	"github.com/ava-labs/avalanchego/vms/rpcdagvm/messenger"

	aliasreaderpb "github.com/ava-labs/avalanchego/proto/pb/aliasreader"
	appsenderpb "github.com/ava-labs/avalanchego/proto/pb/appsender"
	vmpb "github.com/ava-labs/avalanchego/proto/pb/dagvm"
	httppb "github.com/ava-labs/avalanchego/proto/pb/http"
	keystorepb "github.com/ava-labs/avalanchego/proto/pb/keystore"
	messengerpb "github.com/ava-labs/avalanchego/proto/pb/messenger"
	rpcdbpb "github.com/ava-labs/avalanchego/proto/pb/rpcdb"
	sharedmemorypb "github.com/ava-labs/avalanchego/proto/pb/sharedmemory"
	subnetlookuppb "github.com/ava-labs/avalanchego/proto/pb/subnetlookup"
)

var _ vmpb.DAGVMServer = &VMServer{}

// VMServer is a VM that is managed over RPC.
type VMServer struct {
	vmpb.UnsafeDAGVMServer

	vm   vertex.DAGVM

	dbManager      manager.Manager

	serverCloser grpcutils.ServerCloser
	connCloser   wrappers.Closer

	ctx    *snow.Context
	closed chan struct{}
}

// NewServer returns a vm instance connected to a remote vm instance
func NewServer(vm vertex.DAGVM) *VMServer {
	return &VMServer{
		vm:   vm,
	}
}

func (vm *VMServer) Initialize(_ context.Context, req *vmpb.InitializeRequest) (*emptypb.Empty, error) {
	subnetID, err := ids.ToID(req.SubnetId) 
	if err != nil {
		return &emptypb.Empty{}, err
	}
	chainID, err := ids.ToID(req.ChainId)
	if err != nil {
		return &emptypb.Empty{}, err
	}
	nodeID, err := ids.ToNodeID(req.NodeId)
	if err != nil {
		return &emptypb.Empty{}, err
	}
	xChainID, err := ids.ToID(req.XChainId)
	if err != nil {
		return &emptypb.Empty{}, err
	}
	avaxAssetID, err := ids.ToID(req.AvaxAssetId)
	if err != nil {
		return &emptypb.Empty{}, err
	}

	registerer := prometheus.NewRegistry()

	// Current state of process metrics
	processCollector := collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})
	if err := registerer.Register(processCollector); err != nil {
		return &emptypb.Empty{}, err
	}

	// Go process metrics using debug.GCStats
	goCollector := collectors.NewGoCollector()
	if err := registerer.Register(goCollector); err != nil {
		return &emptypb.Empty{}, err
	}

	// gRPC client metrics
	grpcClientMetrics := grpc_prometheus.NewClientMetrics()
	if err := registerer.Register(grpcClientMetrics); err != nil {
		return &emptypb.Empty{}, err
	}

	// Dial each database in the request and construct the database manager
	versionedDBs := make([]*manager.VersionedDatabase, len(req.DbServers))
	for i, vDBReq := range req.DbServers {
		version, err := version.Parse(vDBReq.Version)
		if err != nil {
			// Ignore closing errors to return the original error
			_ = vm.connCloser.Close()
			return &emptypb.Empty{}, err
		}

		clientConn, err := grpcutils.Dial(vDBReq.ServerAddr, grpcutils.DialOptsWithMetrics(grpcClientMetrics)...)
		if err != nil {
			// Ignore closing errors to return the original error
			_ = vm.connCloser.Close()
			return &emptypb.Empty{}, err
		}
		vm.connCloser.Add(clientConn)
		db := rpcdb.NewClient(rpcdbpb.NewDatabaseClient(clientConn))
		versionedDBs[i] = &manager.VersionedDatabase{
			Database: corruptabledb.New(db),
			Version:  version,
		}
	}
	dbManager, err := manager.NewManagerFromDBs(versionedDBs)
	if err != nil {
		// Ignore closing errors to return the original error
		_ = vm.connCloser.Close()
		return &emptypb.Empty{}, err
	}
	vm.dbManager = dbManager

	clientConn, err := grpcutils.Dial(req.ServerAddr, grpcutils.DialOptsWithMetrics(grpcClientMetrics)...)
	if err != nil {
		// Ignore closing errors to return the original error
		_ = vm.connCloser.Close()
		return &emptypb.Empty{}, err
	}

	vm.connCloser.Add(clientConn)

	msgClient := messenger.NewClient(messengerpb.NewMessengerClient(clientConn))
	keystoreClient := gkeystore.NewClient(keystorepb.NewKeystoreClient(clientConn))
	sharedMemoryClient := gsharedmemory.NewClient(sharedmemorypb.NewSharedMemoryClient(clientConn))
	bcLookupClient := galiasreader.NewClient(aliasreaderpb.NewAliasReaderClient(clientConn))
	snLookupClient := gsubnetlookup.NewClient(subnetlookuppb.NewSubnetLookupClient(clientConn))
	appSenderClient := appsender.NewClient(appsenderpb.NewAppSenderClient(clientConn))

	toEngine := make(chan common.Message, 1)
	vm.closed = make(chan struct{})
	go func() {
		for {
			select {
			case msg, ok := <-toEngine:
				if !ok {
					return
				}
				// Nothing to do with the error within the goroutine
				_ = msgClient.Notify(msg)
			case <-vm.closed:
				return
			}
		}
	}()

	vm.ctx = &snow.Context{
		NetworkID: req.NetworkId,
		SubnetID:  subnetID,
		ChainID:   chainID,
		NodeID:    nodeID,

		XChainID:    xChainID,
		AVAXAssetID: avaxAssetID,

		Log:          logging.NoLog{},
		Keystore:     keystoreClient,
		SharedMemory: sharedMemoryClient,
		BCLookup:     bcLookupClient,
		SNLookup:     snLookupClient,
		Metrics:      metrics.NewOptionalGatherer(),
	}

	if err := vm.vm.Initialize(vm.ctx, dbManager, req.GenesisBytes, req.UpgradeBytes, req.ConfigBytes, toEngine, nil, appSenderClient); err != nil {
		// Ignore errors closing resources to return the original error
		_ = vm.connCloser.Close()
		close(vm.closed)
		return &emptypb.Empty{}, err
	}
	
	return &emptypb.Empty{}, nil
}

func (vm *VMServer) SetState(_ context.Context, req *vmpb.SetStateRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, vm.vm.SetState(snow.State(req.State))
}

func (vm *VMServer) GetTx(_ context.Context, req *vmpb.GetTxRequest) (*vmpb.GetTxResponse, error) {
	id, err := ids.ToID(req.Id)
	if err != nil {
		return nil, err
	}
	tx, err := vm.vm.GetTx(id)
	if err != nil {
		return nil, err
	}

	return &vmpb.GetTxResponse{
		Id: req.Id,
		Bytes: tx.Bytes(),
		Status: uint32(tx.Status()),
	}, nil
}

func (vm *VMServer) PendingTxs(context.Context, *emptypb.Empty) (*vmpb.PendingTxsResponse, error) {
	res := vm.vm.PendingTxs()

	var txs []*vmpb.GetTxResponse

	for _, tx := range res {
		id, err := tx.ID().MarshalText()

		if err != nil {
			return nil, err
		}

		txs = append(txs, &vmpb.GetTxResponse{
			Id: id,
			Bytes: tx.Bytes(),
			Status: uint32(tx.Status()),
		})
	}

	return &vmpb.PendingTxsResponse{
		Response: txs,
	}, nil
}

func (vm *VMServer) ParseTx(_ context.Context, req *vmpb.ParseTxRequest) (*vmpb.ParseTxResponse, error) {
	tx, err := vm.vm.ParseTx(req.Tx)

	if err != nil {
		return nil, err
	}

	id, _ := tx.ID().MarshalText()

	return &vmpb.ParseTxResponse{
		Response: &vmpb.GetTxResponse{
			Id: id,
			Bytes: tx.Bytes(),
			Status: uint32(tx.Status()),
		},
	}, nil
}

func (vm *VMServer) TxAccept(_ context.Context, req *vmpb.TxAcceptRequest) (*emptypb.Empty, error) {
	id, err := ids.ToID(req.Id)
	if err != nil {
		return nil, err
	}
	tx, err := vm.vm.GetTx(id)
	if err != nil {
		return nil, err
	}
	if err := tx.Accept(); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (vm *VMServer) TxReject(_ context.Context, req *vmpb.TxRejectRequest) (*emptypb.Empty, error) {
	id, err := ids.ToID(req.Id)
	if err != nil {
		return nil, err
	}
	tx, err := vm.vm.GetTx(id)
	if err != nil {
		return nil, err
	}
	if err := tx.Reject(); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (vm *VMServer) TxVerify(_ context.Context, req *vmpb.TxVerifyRequest) (*emptypb.Empty, error) {
	id, err := ids.ToID(req.Id)
	if err != nil {
		return nil, err
	}
	tx, err := vm.vm.GetTx(id)
	if err != nil {
		return nil, err
	}
	if err := tx.Verify(); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (vm *VMServer) TxDependencies(ctx context.Context, req *vmpb.TxDependenciesRequest) (*vmpb.TxDependenciesResponse, error) {
	id, err := ids.ToID(req.Id)
	if err != nil {
		return nil, err
	}
	tx, err := vm.vm.GetTx(id)
	if err != nil {
		return nil, err
	}

	deps, err := tx.Dependencies()

	if err != nil {
		return nil, err
	}

	var txs []*vmpb.GetTxResponse

	for _, dep := range deps {
		tx, err := vm.ParseTx(ctx, &vmpb.ParseTxRequest{
			Tx: dep.Bytes(),
		})

		if err != nil {
			return nil, err
		}

		txs = append(txs, tx.Response)
	}

	return &vmpb.TxDependenciesResponse{
		Response: txs,
	}, nil
}

func (vm *VMServer) TxWhitelist(_ context.Context, req *vmpb.TxWhitelistRequest) (*vmpb.TxWhitelistResponse, error) {
	// TODO
	return nil, nil
}

func (vm *VMServer) TxHasWhitelist(_ context.Context, req *vmpb.TxHasWhitelistRequest) (*vmpb.TxHasWhitelistResponse, error) {
	// TODO
	return nil, nil
}

func (vm *VMServer) TxInputIDs(_ context.Context, req *vmpb.TxInputIDsRequest) (*vmpb.TxInputIDsResponse, error) {
	id, err := ids.ToID(req.Id)
	if err != nil {
		return nil, err
	}
	tx, err := vm.vm.GetTx(id)
	if err != nil {
		return nil, err
	}

	inputIDs := tx.InputIDs()
	var byteList [][]byte

	for _, id := range inputIDs {
		bytes, err := id.MarshalText()

		if err != nil {
			return nil, err
		}

		byteList = append(byteList, bytes)
	}

	return &vmpb.TxInputIDsResponse{
		Id: byteList,
	}, nil
}

func (vm *VMServer) Shutdown(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
	if vm.closed == nil {
		return &emptypb.Empty{}, nil
	}
	errs := wrappers.Errs{}
	errs.Add(vm.vm.Shutdown())
	close(vm.closed)
	vm.serverCloser.Stop()
	errs.Add(vm.connCloser.Close())
	return &emptypb.Empty{}, errs.Err
}

func (vm *VMServer) CreateHandlers(context.Context, *emptypb.Empty) (*vmpb.CreateHandlersResponse, error) {
	handlers, err := vm.vm.CreateHandlers()
	if err != nil {
		return nil, err
	}
	resp := &vmpb.CreateHandlersResponse{}
	for prefix, h := range handlers {
		handler := h

		serverListener, err := grpcutils.NewListener()
		if err != nil {
			return nil, err
		}
		serverAddr := serverListener.Addr().String()

		// Start the gRPC server which serves the HTTP service
		go grpcutils.Serve(serverListener, func(opts []grpc.ServerOption) *grpc.Server {
			if len(opts) == 0 {
				opts = append(opts, grpcutils.DefaultServerOptions...)
			}
			server := grpc.NewServer(opts...)
			vm.serverCloser.Add(server)
			httppb.RegisterHTTPServer(server, ghttp.NewServer(handler.Handler))
			return server
		})

		resp.Handlers = append(resp.Handlers, &vmpb.Handler{
			Prefix:      prefix,
			LockOptions: uint32(handler.LockOptions),
			ServerAddr:  serverAddr,
		})
	}
	return resp, nil
}

func (vm *VMServer) CreateStaticHandlers(context.Context, *emptypb.Empty) (*vmpb.CreateStaticHandlersResponse, error) {
	handlers, err := vm.vm.CreateStaticHandlers()
	if err != nil {
		return nil, err
	}
	resp := &vmpb.CreateStaticHandlersResponse{}
	for prefix, h := range handlers {
		handler := h

		serverListener, err := grpcutils.NewListener()
		if err != nil {
			return nil, err
		}
		serverAddr := serverListener.Addr().String()

		// Start the gRPC server which serves the HTTP service
		go grpcutils.Serve(serverListener, func(opts []grpc.ServerOption) *grpc.Server {
			if len(opts) == 0 {
				opts = append(opts, grpcutils.DefaultServerOptions...)
			}
			server := grpc.NewServer(opts...)
			vm.serverCloser.Add(server)
			httppb.RegisterHTTPServer(server, ghttp.NewServer(handler.Handler))
			return server
		})

		resp.Handlers = append(resp.Handlers, &vmpb.Handler{
			Prefix:      prefix,
			LockOptions: uint32(handler.LockOptions),
			ServerAddr:  serverAddr,
		})
	}
	return resp, nil
}

func (vm *VMServer) Connected(_ context.Context, req *vmpb.ConnectedRequest) (*emptypb.Empty, error) {
	nodeID, err := ids.ToNodeID(req.NodeId)
	if err != nil {
		return nil, err
	}

	peerVersion, err := version.ParseApplication(req.Version)
	if err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, vm.vm.Connected(nodeID, peerVersion)
}

func (vm *VMServer) Disconnected(_ context.Context, req *vmpb.DisconnectedRequest) (*emptypb.Empty, error) {
	nodeID, err := ids.ToNodeID(req.NodeId)
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, vm.vm.Disconnected(nodeID)
}

func (vm *VMServer) Health(ctx context.Context, req *emptypb.Empty) (*vmpb.HealthResponse, error) {
	vmHealth, err := vm.vm.HealthCheck()
	if err != nil {
		return &vmpb.HealthResponse{}, err
	}
	dbHealth, err := vm.dbHealthChecks()
	if err != nil {
		return &vmpb.HealthResponse{}, err
	}
	report := map[string]interface{}{
		"database": dbHealth,
		"health":   vmHealth,
	}

	details, err := json.Marshal(report)
	return &vmpb.HealthResponse{
		Details: details,
	}, err
}

func (vm *VMServer) dbHealthChecks() (interface{}, error) {
	details := make(map[string]interface{}, len(vm.dbManager.GetDatabases()))

	// Check Database health
	for _, client := range vm.dbManager.GetDatabases() {
		// Shared gRPC client don't close
		health, err := client.Database.HealthCheck()
		if err != nil {
			return nil, fmt.Errorf("failed to check db health %q: %w", client.Version.String(), err)
		}
		details[client.Version.String()] = health
	}

	return details, nil
}

func (vm *VMServer) Version(context.Context, *emptypb.Empty) (*vmpb.VersionResponse, error) {
	version, err := vm.vm.Version()
	return &vmpb.VersionResponse{
		Version: version,
	}, err
}

func (vm *VMServer) AppRequest(_ context.Context, req *vmpb.AppRequestMsg) (*emptypb.Empty, error) {
	nodeID, err := ids.ToNodeID(req.NodeId)
	if err != nil {
		return nil, err
	}
	deadline, err := grpcutils.TimestampAsTime(req.Deadline)
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, vm.vm.AppRequest(nodeID, req.RequestId, deadline, req.Request)
}

func (vm *VMServer) AppRequestFailed(_ context.Context, req *vmpb.AppRequestFailedMsg) (*emptypb.Empty, error) {
	nodeID, err := ids.ToNodeID(req.NodeId)
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, vm.vm.AppRequestFailed(nodeID, req.RequestId)
}

func (vm *VMServer) AppResponse(_ context.Context, req *vmpb.AppResponseMsg) (*emptypb.Empty, error) {
	nodeID, err := ids.ToNodeID(req.NodeId)
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, vm.vm.AppResponse(nodeID, req.RequestId, req.Response)
}

func (vm *VMServer) AppGossip(_ context.Context, req *vmpb.AppGossipMsg) (*emptypb.Empty, error) {
	nodeID, err := ids.ToNodeID(req.NodeId)
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, vm.vm.AppGossip(nodeID, req.Msg)
}
