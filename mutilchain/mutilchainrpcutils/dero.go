// Copyright (c) 2018-2021, The Decred developers
// Copyright (c) 2017, Jonathan Chappelow
// See LICENSE for details.

package mutilchainrpcutils

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/decred/dcrdata/v8/mutilchain/structure"
	"github.com/decred/dcrdata/v8/mutilchain/structure/derostructure"
	"github.com/decred/dcrdata/v8/mutilchain/structure/derostructure/crypto"
	"github.com/ybbus/jsonrpc"
)

// TODO , create on config
var endpoint = "localhost:10102"

func GetDeroBestBlock(client jsonrpc.RPCClient) (string, int64, error) {
	lastBlockResponse, lastErr := client.Call("getlastblockheader")
	if lastErr != nil {
		return "", 0, lastErr
	}
	var lastBlock derostructure.GetBlockHeaderByHash_Result
	lastErr = lastBlockResponse.GetObject(&lastBlock)
	if lastErr != nil {
		return "", 0, lastErr
	}
	return lastBlock.Block_Header.Hash, lastBlock.Block_Header.Height, nil
}

func GetBlockResultByHeight(client jsonrpc.RPCClient, height uint64) (*derostructure.GetBlock_Result, error) {
	response, err := client.Call("getblock", map[string]interface{}{"height": height})
	if err != nil {
		return nil, err
	}
	var bresult derostructure.GetBlock_Result
	err = response.GetObject(&bresult)
	if err != nil {
		return nil, err
	}
	return &bresult, nil
}

func GetBlockResultByHash(client jsonrpc.RPCClient, hash string) (*derostructure.GetBlock_Result, error) {
	response, err := client.Call("getblock", map[string]interface{}{"hash": hash})
	if err != nil {
		return nil, err
	}
	var bresult derostructure.GetBlock_Result
	err = response.GetObject(&bresult)
	if err != nil {
		return nil, err
	}
	return &bresult, nil
}

func GetTransactionByHash(client jsonrpc.RPCClient, hash string) (*derostructure.GetTransaction_Result, error) {
	fmt.Println("hash: ", hash)
	hashArray := make([]string, 0)
	hashArray = append(hashArray, hash)
	response, err := client.Call("gettransactions", map[string]interface{}{"txs_hashes": hashArray, "decode_as_json": 1})
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(response)
	fmt.Println("Get transction: ", string(b))
	var tresult derostructure.GetTransaction_Result
	err = response.GetObject(&tresult)
	if err != nil {
		return nil, err
	}
	return &tresult, nil
}

func GetDeroBlock(client jsonrpc.RPCClient, height uint64) (*structure.MsgBlock, string, error) {
	bresult, err := GetBlockResultByHeight(client, height)
	if err != nil {
		return nil, "", err
	}
	//get difficulty
	difficulty, _ := strconv.ParseInt(bresult.Block_Header.Difficulty, 0, 32)
	blockData := &structure.MsgBlock{
		Hash:       bresult.Block_Header.Hash,
		Height:     int64(height),
		Time:       int64(bresult.Block_Header.Timestamp),
		Nonce:      bresult.Block_Header.Nonce,
		Difficulty: difficulty,
	}
	//Get prev hash
	if height > 1 {
		prevBlockRslt, prevErr := GetBlockResultByHeight(client, height-1)
		if prevErr == nil {
			blockData.PrevHash = prevBlockRslt.Block_Header.Hash
		}
	}

	var deroBlock derostructure.Block
	err = json.Unmarshal([]byte(bresult.Json), &deroBlock)
	if err != nil {
		log.Infof("Parse json block failed")
		return nil, "", err
	}
	b, _ := json.Marshal(deroBlock)
	fmt.Println(string(b))
	txHash := deroBlock.Miner_TX.GetHash()
	if txHash != nil {
		LoadTxFromRpc(client, &blockData.Mtx, txHash.String())
	}
	fmt.Println("txhash: ", txHash)
	blockData.Mtx.Amount = fmt.Sprintf("%.012f", float64(bresult.Block_Header.Reward)/1000000000000)
	blockData.Tx_Count = len(deroBlock.Tx_hashes)
	fees := uint64(0)
	size := float64(0)
	fmt.Println("Block : ", height, ", tx num: ", len(deroBlock.Tx_hashes))
	for i := 0; i < len(deroBlock.Tx_hashes); i++ {
		var tx structure.TxInfo
		err = LoadTxFromRpc(client, &tx, deroBlock.Tx_hashes[i].String())
		if err != nil {
			log.Infof("Get transaction failed")
			continue
		}
		if tx.ValidBlock != bresult.Block_Header.Hash {
			tx.Skipped = true
		}
		blockData.Transactions = append(blockData.Transactions, tx)
		fees += tx.Feeuint64
		size += tx.Size
	}

	blockData.Size = size / 1024
	return blockData, blockData.Hash, nil
}

func LoadTxFromRpc(client jsonrpc.RPCClient, info *structure.TxInfo, txhash string) (err error) {
	tx_result, err := GetTransactionByHash(client, txhash)
	b, _ := json.Marshal(tx_result)
	fmt.Println("Check result: ", string(b))
	if err != nil {
		fmt.Println(fmt.Sprintf("error get transaction: %v", err))
		return err
	}

	if tx_result.Status != "OK" {
		return fmt.Errorf("No Such TX RPC error status %s", tx_result.Status)
	}
	var tx derostructure.Transaction
	if len(tx_result.Txs_as_hex[0]) < 50 {
		return
	}

	info.Hex = tx_result.Txs_as_hex[0]

	tx_bin, _ := hex.DecodeString(tx_result.Txs_as_hex[0])
	fmt.Println("Hex: ", string(tx_bin))
	err = tx.DeserializeHeader(tx_bin)
	if err != nil {
		fmt.Println("Error here", err)
		return
	}
	fmt.Println("tx decoded: ", tx.Version)
	// fill as much info required from headers
	if tx_result.Txs[0].In_pool {
		return
	} else {
		info.Height = tx_result.Txs[0].Block_Height
	}

	if tx.IsCoinbase() { // fill miner tx reward from what the chain tells us
		info.Amount = fmt.Sprintf("%.012f", float64(uint64(tx_result.Txs[0].Reward))/1000000000000)
	}

	info.ValidBlock = tx_result.Txs[0].ValidBlock
	info.InvalidBlock = tx_result.Txs[0].InvalidBlock
	return LoadTxInfoFromTx(client, info, &tx)
}

func LoadTxInfoFromTx(client jsonrpc.RPCClient, info *structure.TxInfo, tx *derostructure.Transaction) (err error) {
	info.Hash = tx.GetHash().String()
	info.Size = float64(len(tx.Serialize())) / 1024
	info.Version = int(tx.Version)
	info.In = len(tx.Vin)
	info.Out = len(tx.Vout)

	if tx.Parse_Extra() {
		// store public key if present
		if _, ok := tx.Extra_map[derostructure.TX_PUBLIC_KEY]; ok {
			info.TXpublickey = tx.Extra_map[derostructure.TX_PUBLIC_KEY].(crypto.Key).String()
		}
	}

	if !tx.IsCoinbase() {
		info.Fee = fmt.Sprintf("%.012f", float64(tx.RctSignature.Get_TX_Fee())/1000000000000)
		info.Feeuint64 = tx.RctSignature.Get_TX_Fee()
		info.Amount = "?"
	} else {
		info.CoinBase = true
		info.In = 0
	}

	blockInfo, blockErr := GetBlockResultByHeight(client, uint64(info.Height))
	if blockErr == nil {
		info.Time = int64(blockInfo.Block_Header.Timestamp)
		info.LockTime = blockInfo.Block_Header.Height
	}
	//get txout info
	vouts := make([]structure.TxOut, 0)
	for _, txout := range tx.Vout {
		vout := structure.TxOut{
			Value:    int64(txout.Amount),
			PkScript: txout.Target.(derostructure.Txout_to_script).Script,
			Address:  txout.Target.(derostructure.Txout_to_key).Key.String(),
		}
		vouts = append(vouts, vout)
	}
	info.TxOut = vouts

	//Get txin info
	vins := make([]structure.TxIn, 0)
	for _, txin := range tx.Vin {
		vin := structure.TxIn{
			Value:           int64(txin.(derostructure.Txin_to_key).Amount),
			SignatureScript: txin.(derostructure.Txin_to_script).Sigset,
		}
		prevOut := structure.OutPoint{
			Hash:  fmt.Sprintf("%x", txin.(derostructure.Txin_to_scripthash).Script.Script),
			Index: uint32(txin.(derostructure.Txin_to_script).Prevout),
		}
		vin.PreviousOutPoint = prevOut
		vins = append(vins, vin)
	}
	info.TxIn = vins
	return nil
}
