// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package rpcdagvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"

	"github.com/hashicorp/go-plugin"

	"github.com/prometheus/client_golang/prometheus"

	"go.uber.org/zap"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/protobuf/types/known/emptypb"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/ava-labs/avalanchego/api/keystore/gkeystore"
	"github.com/ava-labs/avalanchego/api/metrics"
	"github.com/ava-labs/avalanchego/chains/atomic/gsharedmemory"
	"github.com/ava-labs/avalanchego/database/manager"
	"github.com/ava-labs/avalanchego/database/rpcdb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/ids/galiasreader"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowstorm"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/common/appsender"
	"github.com/ava-labs/avalanchego/utils/resource"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/avalanchego/vms/components/chain"
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

var (
	errUnsupportedFXs                       = errors.New("unsupported feature extensions")
)

// VMClient is an implementation of a VM that talks over RPC.
type VMClient struct {
	*chain.State
	client         vmpb.DAGVMClient
	proc           *plugin.Client
	pid            int
	processTracker resource.ProcessTracker

	messenger    *messenger.Server
	keystore     *gkeystore.Server
	sharedMemory *gsharedmemory.Server
	bcLookup     *galiasreader.Server
	snLookup     *gsubnetlookup.Server
	appSender    *appsender.Server

	serverCloser grpcutils.ServerCloser
	conns        []*grpc.ClientConn

	grpcServerMetrics *grpc_prometheus.ServerMetrics

	ctx *snow.Context
}

// NewClient returns a VM connected to a remote VM
func NewClient(client vmpb.DAGVMClient) *VMClient {
	return &VMClient{
		client: client,
	}
}

// SetProcess gives ownership of the server process to the client.
func (vm *VMClient) SetProcess(ctx *snow.Context, proc *plugin.Client, processTracker resource.ProcessTracker) {
	vm.ctx = ctx
	vm.proc = proc
	vm.processTracker = processTracker
	vm.pid = proc.ReattachConfig().Pid
	processTracker.TrackProcess(vm.pid)
}

func (vm *VMClient) Initialize(
	ctx *snow.Context,
	dbManager manager.Manager,
	genesisBytes []byte,
	upgradeBytes []byte,
	configBytes []byte,
	toEngine chan<- common.Message,
	fxs []*common.Fx,
	appSender common.AppSender,
) error {
	if len(fxs) != 0 {
		return errUnsupportedFXs
	}

	vm.ctx = ctx

	// Register metrics
	registerer := prometheus.NewRegistry()
	multiGatherer := metrics.NewMultiGatherer()
	vm.grpcServerMetrics = grpc_prometheus.NewServerMetrics()
	if err := registerer.Register(vm.grpcServerMetrics); err != nil {
		return err
	}
	if err := multiGatherer.Register("rpcdagvm", registerer); err != nil {
		return err
	}

	// Initialize and serve each database and construct the db manager
	// initialize request parameters
	versionedDBs := dbManager.GetDatabases()
	versionedDBServers := make([]*vmpb.VersionedDBServer, len(versionedDBs))
	for i, semDB := range versionedDBs {
		db := rpcdb.NewServer(semDB.Database)
		dbVersion := semDB.Version.String()
		serverListener, err := grpcutils.NewListener()
		if err != nil {
			return err
		}
		serverAddr := serverListener.Addr().String()

		go grpcutils.Serve(serverListener, vm.getDBServerFunc(db))
		vm.ctx.Log.Info("grpc: serving database",
			zap.String("version", dbVersion),
			zap.String("address", serverAddr),
		)

		versionedDBServers[i] = &vmpb.VersionedDBServer{
			ServerAddr: serverAddr,
			Version:    dbVersion,
		}
	}

	vm.messenger = messenger.NewServer(toEngine)
	vm.keystore = gkeystore.NewServer(ctx.Keystore)
	vm.sharedMemory = gsharedmemory.NewServer(ctx.SharedMemory, dbManager.Current().Database)
	vm.bcLookup = galiasreader.NewServer(ctx.BCLookup)
	vm.snLookup = gsubnetlookup.NewServer(ctx.SNLookup)
	vm.appSender = appsender.NewServer(appSender)

	serverListener, err := grpcutils.NewListener()
	if err != nil {
		return err
	}
	serverAddr := serverListener.Addr().String()

	go grpcutils.Serve(serverListener, vm.getInitServer)
	vm.ctx.Log.Info("grpc: serving vm services",
		zap.String("address", serverAddr),
	)

	vm.client.Initialize(context.Background(), &vmpb.InitializeRequest{
		NetworkId:    ctx.NetworkID,
		SubnetId:     ctx.SubnetID[:],
		ChainId:      ctx.ChainID[:],
		NodeId:       ctx.NodeID.Bytes(),
		XChainId:     ctx.XChainID[:],
		AvaxAssetId:  ctx.AVAXAssetID[:],
		GenesisBytes: genesisBytes,
		UpgradeBytes: upgradeBytes,
		ConfigBytes:  configBytes,
		DbServers:    versionedDBServers,
		ServerAddr:   serverAddr,
	})

	return nil
}

func (vm *VMClient) getDBServerFunc(db rpcdbpb.DatabaseServer) func(opts []grpc.ServerOption) *grpc.Server { // #nolint
	return func(opts []grpc.ServerOption) *grpc.Server {
		if len(opts) == 0 {
			opts = append(opts, grpcutils.DefaultServerOptions...)
		}

		// Collect gRPC serving metrics
		opts = append(opts, grpc.UnaryInterceptor(vm.grpcServerMetrics.UnaryServerInterceptor()))
		opts = append(opts, grpc.StreamInterceptor(vm.grpcServerMetrics.StreamServerInterceptor()))

		server := grpc.NewServer(opts...)

		grpcHealth := health.NewServer()
		// The server should use an empty string as the key for server's overall
		// health status.
		// See https://github.com/grpc/grpc/blob/master/doc/health-checking.md
		grpcHealth.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

		vm.serverCloser.Add(server)

		// register database service
		rpcdbpb.RegisterDatabaseServer(server, db)
		// register health service
		healthpb.RegisterHealthServer(server, grpcHealth)

		// Ensure metric counters are zeroed on restart
		grpc_prometheus.Register(server)

		return server
	}
}

func (vm *VMClient) getInitServer(opts []grpc.ServerOption) *grpc.Server {
	if len(opts) == 0 {
		opts = append(opts, grpcutils.DefaultServerOptions...)
	}

	// Collect gRPC serving metrics
	opts = append(opts, grpc.UnaryInterceptor(vm.grpcServerMetrics.UnaryServerInterceptor()))
	opts = append(opts, grpc.StreamInterceptor(vm.grpcServerMetrics.StreamServerInterceptor()))

	server := grpc.NewServer(opts...)

	grpcHealth := health.NewServer()
	// The server should use an empty string as the key for server's overall
	// health status.
	// See https://github.com/grpc/grpc/blob/master/doc/health-checking.md
	grpcHealth.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	vm.serverCloser.Add(server)

	// register the messenger service
	messengerpb.RegisterMessengerServer(server, vm.messenger)
	// register the keystore service
	keystorepb.RegisterKeystoreServer(server, vm.keystore)
	// register the shared memory service
	sharedmemorypb.RegisterSharedMemoryServer(server, vm.sharedMemory)
	// register the blockchain alias service
	aliasreaderpb.RegisterAliasReaderServer(server, vm.bcLookup)
	// register the subnet alias service
	subnetlookuppb.RegisterSubnetLookupServer(server, vm.snLookup)
	// register the app sender service
	appsenderpb.RegisterAppSenderServer(server, vm.appSender)
	// register the health service
	healthpb.RegisterHealthServer(server, grpcHealth)

	// Ensure metric counters are zeroed on restart
	grpc_prometheus.Register(server)

	return server
}

func (vm *VMClient) SetState(state snow.State) error {
	_, err := vm.client.SetState(context.Background(), &vmpb.SetStateRequest{
		State: uint32(state),
	})
	if err != nil {
		return err
	}

	return nil
}

func (vm *VMClient) GetTx(id ids.ID) (snowstorm.Tx, error) {
	resp, err := vm.client.GetTx(context.Background(), &vmpb.GetTxRequest{
		Id: id[:],
	})

	if err != nil {
		return nil, err
	}

	status := choices.Status(resp.Status)
	if err := status.Valid(); err != nil {
		return nil, err
	}

	return &txClient{
		vm: vm,
		id: id,
		status: status,
		bytes: resp.Bytes,
	}, nil
}

func (vm *VMClient) PendingTxs() ([]snowstorm.Tx) {
	resp, _ := vm.client.PendingTxs(context.Background(), &emptypb.Empty{})

	var txs []snowstorm.Tx
	for _, tx := range resp.Response {
		status := choices.Status(tx.Status)
		if err := status.Valid(); err != nil {
			return nil
		}

		id, err := ids.ToID(tx.Id)

		if err != nil {
			return nil
		}

		txs = append(txs, &txClient{
			vm: vm,
			id: id,
			status: status,
			bytes: tx.Bytes,
		})
	}

	return txs
}

func (vm *VMClient) ParseTx(tx []byte) (snowstorm.Tx, error) {
	resp, err := vm.client.ParseTx(context.Background(), &vmpb.ParseTxRequest{
		Tx: tx,
	})

	if err != nil {
		return nil, err
	}

	id, err := ids.ToID(resp.Id)

	if err != nil {
		return nil, err
	}

	status := choices.Status(resp.Response.Status)
	if err := status.Valid(); err != nil {
		return nil, err
	}

	return &txClient{
		vm: vm,
		id: id,
		status: status,
		bytes: tx,
	}, nil
}

type txClient struct {
	vm *VMClient

	id	     ids.ID
	status   choices.Status
	bytes    []byte
}

func (tx *txClient) Accept() error {
	tx.status = choices.Accepted
	_, err := tx.vm.client.TxAccept(context.Background(), &vmpb.TxAcceptRequest{
		Id: tx.id[:],
	})
	return err
}

func (tx *txClient) Reject() error {
	tx.status = choices.Rejected
	_, err := tx.vm.client.TxReject(context.Background(), &vmpb.TxRejectRequest{
		Id: tx.id[:],
	})
	return err
}

func (tx *txClient) Verify() error {
	_, err := tx.vm.client.TxVerify(context.Background(), &vmpb.TxVerifyRequest{
		Id: tx.id[:],
	})
	if err != nil {
		return err
	}

	return nil
}

func (tx *txClient) Dependencies() ([]snowstorm.Tx, error) {
	resp, err := tx.vm.client.TxDependencies(context.Background(), &vmpb.TxDependenciesRequest{
		Id: tx.id[:],
	})

	if(err != nil) {
		return nil, err
	}

	var res []snowstorm.Tx

	for _, txResp := range resp.Response {
		if err != nil {
			return nil, err
		}
	
		status := choices.Status(txResp.Status)
		if err := status.Valid(); err != nil {
			return nil, err
		}
	
		res = append(res, &txClient{
			vm: tx.vm,
			status: status,
			bytes: txResp.Bytes,
		})
	}

	return res, nil
}

func (tx *txClient) Whitelist() (ids.Set, error) {
	return nil, nil
}

func (tx *txClient) HasWhitelist() bool {
	return false
}

func (tx *txClient) InputIDs() []ids.ID {
	return nil
}

func (tx *txClient) ID() ids.ID {
	return tx.id
}

func (tx *txClient) Status() choices.Status {
	return tx.status
}

func (tx *txClient) Bytes() []byte {
	return tx.bytes
}

func (vm *VMClient) Shutdown() error {
	errs := wrappers.Errs{}
	_, err := vm.client.Shutdown(context.Background(), &emptypb.Empty{})
	errs.Add(err)

	vm.serverCloser.Stop()
	for _, conn := range vm.conns {
		errs.Add(conn.Close())
	}

	vm.proc.Kill()
	vm.processTracker.UntrackProcess(vm.pid)
	return errs.Err
}

func (vm *VMClient) CreateHandlers() (map[string]*common.HTTPHandler, error) {
	resp, err := vm.client.CreateHandlers(context.Background(), &emptypb.Empty{})
	if err != nil {
		return nil, err
	}

	handlers := make(map[string]*common.HTTPHandler, len(resp.Handlers))
	for _, handler := range resp.Handlers {
		clientConn, err := grpcutils.Dial(handler.ServerAddr)
		if err != nil {
			return nil, err
		}

		vm.conns = append(vm.conns, clientConn)
		handlers[handler.Prefix] = &common.HTTPHandler{
			LockOptions: common.LockOption(handler.LockOptions),
			Handler:     ghttp.NewClient(httppb.NewHTTPClient(clientConn)),
		}
	}
	return handlers, nil
}

func (vm *VMClient) CreateStaticHandlers() (map[string]*common.HTTPHandler, error) {
	resp, err := vm.client.CreateStaticHandlers(context.Background(), &emptypb.Empty{})
	if err != nil {
		return nil, err
	}

	handlers := make(map[string]*common.HTTPHandler, len(resp.Handlers))
	for _, handler := range resp.Handlers {
		clientConn, err := grpcutils.Dial(handler.ServerAddr)
		if err != nil {
			return nil, err
		}

		vm.conns = append(vm.conns, clientConn)
		handlers[handler.Prefix] = &common.HTTPHandler{
			LockOptions: common.LockOption(handler.LockOptions),
			Handler:     ghttp.NewClient(httppb.NewHTTPClient(clientConn)),
		}
	}
	return handlers, nil
}

func (vm *VMClient) Connected(nodeID ids.NodeID, nodeVersion *version.Application) error {
	_, err := vm.client.Connected(context.Background(), &vmpb.ConnectedRequest{
		NodeId:  nodeID[:],
		Version: nodeVersion.String(),
	})
	return err
}

func (vm *VMClient) Disconnected(nodeID ids.NodeID) error {
	_, err := vm.client.Disconnected(context.Background(), &vmpb.DisconnectedRequest{
		NodeId: nodeID[:],
	})
	return err
}

func (vm *VMClient) HealthCheck() (interface{}, error) {
	health, err := vm.client.Health(context.Background(), &emptypb.Empty{})
	if err != nil {
		return nil, fmt.Errorf("health check failed: %w", err)
	}

	return json.RawMessage(health.Details), nil
}

func (vm *VMClient) Version() (string, error) {
	resp, err := vm.client.Version(
		context.Background(),
		&emptypb.Empty{},
	)
	if err != nil {
		return "", err
	}
	return resp.Version, nil
}

func (vm *VMClient) AppRequest(nodeID ids.NodeID, requestID uint32, deadline time.Time, request []byte) error {
	_, err := vm.client.AppRequest(
		context.Background(),
		&vmpb.AppRequestMsg{
			NodeId:    nodeID[:],
			RequestId: requestID,
			Request:   request,
			Deadline:  grpcutils.TimestampFromTime(deadline),
		},
	)
	return err
}

func (vm *VMClient) AppResponse(nodeID ids.NodeID, requestID uint32, response []byte) error {
	_, err := vm.client.AppResponse(
		context.Background(),
		&vmpb.AppResponseMsg{
			NodeId:    nodeID[:],
			RequestId: requestID,
			Response:  response,
		},
	)
	return err
}

func (vm *VMClient) AppRequestFailed(nodeID ids.NodeID, requestID uint32) error {
	_, err := vm.client.AppRequestFailed(
		context.Background(),
		&vmpb.AppRequestFailedMsg{
			NodeId:    nodeID[:],
			RequestId: requestID,
		},
	)
	return err
}

func (vm *VMClient) AppGossip(nodeID ids.NodeID, msg []byte) error {
	_, err := vm.client.AppGossip(
		context.Background(),
		&vmpb.AppGossipMsg{
			NodeId: nodeID[:],
			Msg:    msg,
		},
	)
	return err
}


