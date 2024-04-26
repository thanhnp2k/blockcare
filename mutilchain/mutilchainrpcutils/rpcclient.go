// Copyright (c) 2018-2021, The Decred developers
// Copyright (c) 2017, Jonathan Chappelow
// See LICENSE for details.

package mutilchainrpcutils

import (
	"fmt"

	"github.com/decred/dcrdata/v8/mutilchain"
	"github.com/decred/dcrdata/v8/mutilchain/structure"
	"github.com/ybbus/jsonrpc"
)

// Get best block, get hash, height
func GetBestBlock(client jsonrpc.RPCClient, chainType string) (string, int64, error) {
	switch chainType {
	case mutilchain.TYPEDERO:
		return GetDeroBestBlock(client)
	default:
		return "", 0, fmt.Errorf("%s", "Get Best block failed because not found blockchain type")
	}
}

func GetBlock(client jsonrpc.RPCClient, height int64, chainType string) (*structure.MsgBlock, string, error) {
	switch chainType {
	case mutilchain.TYPEDERO:
		return GetDeroBlock(client, uint64(height))
	default:
		return nil, "", fmt.Errorf("Get block for height %d failed.", height)
	}
}
