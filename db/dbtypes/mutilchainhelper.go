package dbtypes

import (
	"time"

	"github.com/btcsuite/btcd/btcjson"
	btcClient "github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrdata/v8/mutilchain"
	"github.com/decred/dcrdata/v8/mutilchain/btcrpcutils"
	"github.com/decred/dcrdata/v8/mutilchain/structure"
	"github.com/ybbus/jsonrpc"
)

// MsgBlockToDBBlock creates a dbtypes.Block from a wire.MsgBlock
func MsgMutilchainBlockToDBBlock(client jsonrpc.RPCClient, blockInfo *structure.MsgBlock) *Block {
	// convert each transaction hash to a hex string
	var txHashStrs []string
	for _, transaction := range blockInfo.Transactions {
		txHashStrs = append(txHashStrs, transaction.Hash)
	}
	// Assemble the block
	return &Block{
		Hash:    blockInfo.Hash,
		Size:    uint32(blockInfo.Size * 1000),
		Height:  uint32(blockInfo.Height),
		Version: uint32(blockInfo.Mtx.Version),
		NumTx:   uint32(len(blockInfo.Transactions)),
		// nil []int64 for TxDbIDs
		NumRegTx:     uint32(len(blockInfo.Transactions)),
		Tx:           txHashStrs,
		Time:         NewTimeDef(time.Unix(blockInfo.Time, 0)),
		Nonce:        blockInfo.Nonce,
		Difficulty:   float64(blockInfo.Difficulty),
		PreviousHash: blockInfo.PrevHash,
	}
}

func ExtractMutilchainBlockTransactions(client jsonrpc.RPCClient, dbBlock *Block, blockInfo *structure.MsgBlock,
	chainType string) ([]*Tx, [][]*Vout, []VinTxPropertyARRAY) {
	dbTxs, dbTxVouts, dbTxVins := processMutichainTransactions(client, dbBlock, blockInfo, chainType)
	return dbTxs, dbTxVouts, dbTxVins
}

func GetMutilchainValueInOfTransaction(client *btcClient.Client, vin *wire.TxIn) int64 {
	prevTransaction, err := btcrpcutils.GetRawTransactionByTxidStr(client, vin.PreviousOutPoint.Hash.String())
	if err != nil {
		return 0
	}
	return GetBTCValueInFromRawTransction(prevTransaction, vin)
}

func GetMutichainValueInFromRawTransction(rawTx *btcjson.TxRawResult, vin *wire.TxIn) int64 {
	if rawTx.Vout == nil || len(rawTx.Vout) <= int(vin.PreviousOutPoint.Index) {
		return 0
	}
	return GetMutilchainUnitAmount(rawTx.Vout[vin.PreviousOutPoint.Index].Value, mutilchain.TYPEBTC)
}

func processMutichainTransactions(client jsonrpc.RPCClient, dbBlock *Block, blockInfo *structure.MsgBlock,
	chainType string) ([]*Tx, [][]*Vout, []VinTxPropertyARRAY) {
	var txs = blockInfo.Transactions
	blockHash := blockInfo.Hash
	dbTransactions := make([]*Tx, 0, len(txs))
	dbTxVouts := make([][]*Vout, len(txs))
	dbTxVins := make([]VinTxPropertyARRAY, len(txs))

	for txIndex, tx := range txs {
		var sent int64
		var spent int64
		for _, txout := range tx.TxOut {
			sent += txout.Value
		}
		dbTx := &Tx{
			BlockHash:   blockHash,
			BlockHeight: blockInfo.Height,
			BlockTime:   NewTimeDef(time.Unix(blockInfo.Time, 0)),
			Time:        NewTimeDef(time.Unix(tx.Time, 0)),
			Version:     uint16(tx.Version),
			TxID:        tx.Hash,
			BlockIndex:  uint32(txIndex),
			Locktime:    uint32(tx.LockTime),
			Size:        uint32(tx.Size * 1000),
			Sent:        sent,
			NumVin:      uint32(len(tx.TxIn)),
			NumVout:     uint32(len(tx.TxOut)),
		}
		dbTxVins[txIndex] = make(VinTxPropertyARRAY, 0, len(tx.TxIn))
		for idx, txin := range tx.TxIn {
			spent += txin.Value

			dbTxVins[txIndex] = append(dbTxVins[txIndex], VinTxProperty{
				PrevTxHash:  txin.PreviousOutPoint.Hash,
				PrevTxIndex: txin.PreviousOutPoint.Index,
				ValueIn:     txin.Value,
				TxID:        dbTx.TxID,
				TxIndex:     uint32(idx),
				TxTree:      uint16(dbTx.Tree),
				ScriptSig:   txin.SignatureScript,
			})
		}
		dbTx.Spent = spent
		if spent > 0 {
			dbTx.Fees = dbTx.Spent - dbTx.Sent
		}
		// Vouts and their db IDs
		dbTxVouts[txIndex] = make([]*Vout, 0, len(tx.TxOut))
		//dbTx.Vouts = make([]*Vout, 0, len(tx.TxOut))
		for io, txout := range tx.TxOut {
			vout := Vout{
				TxHash:       dbTx.TxID,
				TxIndex:      uint32(io),
				Value:        uint64(txout.Value),
				ScriptPubKey: txout.PkScript,
			}
			vout.ScriptPubKeyData.Addresses = append(vout.ScriptPubKeyData.Addresses, txout.Address)
			dbTxVouts[txIndex] = append(dbTxVouts[txIndex], &vout)
		}

		//dbTx.VoutDbIds = make([]uint64, len(dbTxVouts[txIndex]))

		dbTransactions = append(dbTransactions, dbTx)
	}

	return dbTransactions, dbTxVouts, dbTxVins
}
