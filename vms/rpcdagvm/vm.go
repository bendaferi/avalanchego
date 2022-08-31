// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package rpcdagvm

import (
	"golang.org/x/net/context"

	"google.golang.org/grpc"

	"github.com/hashicorp/go-plugin"

	"github.com/ava-labs/avalanchego/snow/engine/avalanche/vertex"
	"github.com/ava-labs/avalanchego/vms/rpcdagvm/grpcutils"

	vmpb "github.com/ava-labs/avalanchego/proto/pb/dagvm"
)

// protocolVersion should be bumped anytime changes are made which require
// the plugin vm to upgrade to latest avalanchego release to be compatible.
const protocolVersion = 15

var (
	// Handshake is a common handshake that is shared by plugin and host.
	Handshake = plugin.HandshakeConfig{
		ProtocolVersion:  protocolVersion,
		MagicCookieKey:   "VM_PLUGIN",
		MagicCookieValue: "dynamic",
	}

	// PluginMap is the map of plugins we can dispense.
	PluginMap = map[string]plugin.Plugin{
		"vm": &vmPlugin{},
	}

	_ plugin.Plugin     = &vmPlugin{}
	_ plugin.GRPCPlugin = &vmPlugin{}
)

type vmPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	// Concrete implementation, written in Go. This is only used for plugins
	// that are written in Go.
	vm vertex.DAGVM
}

// New will be called by the server side of the plugin to pass into the server
// side PluginMap for dispatching.
func New(vm vertex.DAGVM) plugin.Plugin {
	return &vmPlugin{vm: vm}
}

// GRPCServer registers a new GRPC server.
func (p *vmPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	vmpb.RegisterDAGVMServer(s, NewServer(p.vm))
	return nil
}

// GRPCClient returns a new GRPC client
func (p *vmPlugin) GRPCClient(ctx context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return NewClient(vmpb.NewDAGVMClient(c)), nil
}

// Serve serves a ChainVM plugin using sane gRPC server defaults.
func Serve(vm vertex.DAGVM) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins: map[string]plugin.Plugin{
			"vm": New(vm),
		},
		// ensure proper defaults
		GRPCServer: grpcutils.NewDefaultServer,
	})
}
